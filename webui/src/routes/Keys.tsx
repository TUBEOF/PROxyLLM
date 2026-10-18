import { FormEvent, useCallback, useMemo, useState } from 'react';
import { apiCall } from '../api';
import { copyText } from '../clipboard';
import type { ClientKey, CreatedKey, KeysResponse } from '../types';
import { CONFIG_LIVE_MS, EmptyState, ErrorNote, PageHead, useLiveData, useMutation } from '../hooks';
import { LiveBadge } from '../components/LiveBadge';
import { Modal } from '../components/Overlays';
import { formatDateTime } from '../time';
import { IconCheck, IconCopy, IconPlus } from '../icons';

// API keys (the auth credential clients present to the proxy): create,
// regenerate, delete. The server returns the full key exactly once — it is
// stored as a one-way keyed hash, so this modal is the only place the secret
// is ever visible. Deleted keys are hard-deleted: only current keys are
// listed, and a deleted key stops authenticating immediately.

type CreatedMode = 'create' | 'regenerate';

export default function Keys() {
  const { data, loading, error, reload, updatedAt } = useLiveData<KeysResponse>(
    (signal) => apiCall<KeysResponse>('GET', '/api/admin/keys', undefined, signal),
    CONFIG_LIVE_MS,
  );
  const keys = useMemo(() => data?.keys ?? [], [data]);

  const [creating, setCreating] = useState(false);
  const [createdMode, setCreatedMode] = useState<CreatedMode>('create');
  const [name, setName] = useState('');
  const [formError, setFormError] = useState<string | null>(null);
  // The display-once secret + its row id, held only while the modal is open.
  const [created, setCreated] = useState<CreatedKey | null>(null);
  const [copied, setCopied] = useState(false);
  const [copyBlocked, setCopyBlocked] = useState(false);

  // Confirmation targets: a destructive action never fires on one click.
  const [confirmDelete, setConfirmDelete] = useState<ClientKey | null>(null);
  const [confirmRegenerate, setConfirmRegenerate] = useState<ClientKey | null>(null);
  // Per-row pending state: one key's action must not disable the other rows.
  const [busy, setBusy] = useState<string | null>(null);

  const create = useMutation();
  const regenerate = useMutation();
  const remove = useMutation();

  const openCreate = useCallback(() => {
    setName('');
    setFormError(null);
    setCreated(null);
    setCreatedMode('create');
    setCreating(true);
  }, []);

  const closeCreate = useCallback(() => {
    // Closing after creation discards the secret — it cannot be shown again.
    setCreating(false);
    setCreated(null);
    setCreatedMode('create');
    setCopied(false);
    setCopyBlocked(false);
    setFormError(null);
    create.clearError();
  }, [create]);

  const submit = useCallback(
    async (e: FormEvent) => {
      e.preventDefault();
      if (name.trim() === '') {
        setFormError('Name is required');
        return;
      }
      setFormError(null);
      const result = await create.run(() => apiCall<CreatedKey>('POST', '/api/admin/keys', { name: name.trim() }));
      if (result !== null) {
        setCreatedMode('create');
        setCreated(result);
        reload();
      }
    },
    [name, create, reload],
  );

  const copy = useCallback(async () => {
    if (created === null) return;
    // Same helper as the device sign-in: the insecure-origin fallback, and a
    // reported failure instead of a silent no-op.
    const outcome = await copyText(created.key);
    setCopied(outcome === 'copied');
    setCopyBlocked(outcome === 'failed');
  }, [created]);

  const doRegenerate = useCallback(
    async (k: ClientKey) => {
      setConfirmRegenerate(null);
      setBusy(k.id);
      const result = await regenerate.run(() =>
        apiCall<CreatedKey>('POST', `/api/admin/keys/${encodeURIComponent(k.id)}/regenerate`),
      );
      setBusy(null);
      if (result !== null) {
        setCreatedMode('regenerate');
        setCreated(result);
        setCreating(true);
        reload();
      }
    },
    [regenerate, reload],
  );

  const doDelete = useCallback(
    async (k: ClientKey) => {
      setConfirmDelete(null);
      setBusy(k.id);
      const ok = await remove.run(() => apiCall('DELETE', `/api/admin/keys/${encodeURIComponent(k.id)}`));
      setBusy(null);
      if (ok) reload();
    },
    [remove, reload],
  );

  return (
    <>
      <PageHead
        title="API keys"
        sub="Keys clients use against both API surfaces. Secrets are shown once and stored as a one-way keyed hash."
        actions={
          <>
            <LiveBadge updatedAt={updatedAt} intervalMs={CONFIG_LIVE_MS} />
            <button type="button" className="btn btn-primary" onClick={openCreate}>
              <IconPlus width="16" height="16" />
              Create key
            </button>
          </>
        }
      />
      <div className="canvas">
        {error !== null ? (
          <div className="w-full">
            <ErrorNote message={error} />
          </div>
        ) : loading ? (
          <div className="w-full">
            <EmptyState title="Loading API keys" body="Fetching the key list." />
          </div>
        ) : keys.length === 0 ? (
          <div className="w-full">
            <EmptyState
              title="No API keys"
              body="Create a key so clients can authenticate against the proxy. Keys are required by default on both API surfaces."
              action={
                <button type="button" className="btn btn-primary btn-sm" onClick={openCreate}>
                  <IconPlus width="14" height="14" />
                  Create the first key
                </button>
              }
            />
          </div>
        ) : (
          <div className="w-full">
            <div className="table-wrap">
              <div className="table-scroll">
                <table className="table">
                  <thead>
                    <tr>
                      <th>Name</th>
                      <th>Prefix</th>
                      <th>Created</th>
                      <th>Last used</th>
                      <th />
                    </tr>
                  </thead>
                  <tbody>
                    {keys.map((k) => (
                      <tr key={k.id}>
                        <td>
                          <span className="primary">{k.name}</span>
                          <br />
                          <span className="mono">{k.id}</span>
                        </td>
                        <td className="mono">{k.key_prefix}</td>
                        <td className="mono">{formatDateTime(k.created_at)}</td>
                        <td className="mono">
                          {k.last_used_at === null || k.last_used_at === undefined
                            ? 'never'
                            : formatDateTime(k.last_used_at)}
                        </td>
                        <td>
                          <span className="row-actions is-visible">
                            <button
                              type="button"
                              className="btn btn-secondary btn-sm"
                              onClick={() => setConfirmRegenerate(k)}
                              disabled={busy === k.id}
                            >
                              Regenerate
                            </button>
                            <button
                              type="button"
                              className="btn btn-danger btn-sm"
                              onClick={() => setConfirmDelete(k)}
                              disabled={busy === k.id}
                            >
                              Delete
                            </button>
                          </span>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
            {(remove.error ?? regenerate.error) !== null && (
              <ErrorNote message={remove.error ?? regenerate.error ?? 'Request failed'} />
            )}
          </div>
        )}
      </div>

      {creating && (
        <Modal
          title={
            created === null ? 'Create API key' : createdMode === 'regenerate' ? 'Key regenerated' : 'Key created'
          }
          sub={created === null ? 'Name the key for the key list and request logs.' : undefined}
          onClose={closeCreate}
          footer={
            created === null ? (
              <>
                <button type="button" className="btn btn-secondary" onClick={closeCreate} disabled={create.pending}>
                  Cancel
                </button>
                <button type="submit" className="btn btn-primary" form="key-form" disabled={create.pending}>
                  Create
                </button>
              </>
            ) : (
              <button type="button" className="btn btn-primary" onClick={closeCreate}>
                Done
              </button>
            )
          }
        >
          {created === null ? (
            <form className="form" id="key-form" onSubmit={submit}>
              {(formError ?? create.error) !== null && (
                <p role="alert" className="form-note">
                  {formError ?? create.error}
                </p>
              )}
              <div className="field">
                <label htmlFor="k-name">Name</label>
                <input
                  id="k-name"
                  className="input"
                  type="text"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="e.g. claude-code-laptop"
                  autoFocus
                  required
                />
                <span className="hint">Shown in the key list and request logs for attribution.</span>
              </div>
            </form>
          ) : (
            <div className="form">
              <div className="field">
                <label>Secret (shown once)</label>
                <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                  <input
                    className="input mono"
                    type="text"
                    value={created.key}
                    readOnly
                    onFocus={(e) => e.target.select()}
                  />
                  <button
                    type="button"
                    className="btn btn-secondary btn-sm"
                    onClick={() => void copy()}
                    aria-label="Copy key to clipboard"
                  >
                    {copied ? <IconCheck width="14" height="14" /> : <IconCopy width="14" height="14" />}
                    {copied ? 'Copied' : 'Copy'}
                  </button>
                </div>
                <span className="hint">
                  This is the only time the full key is visible — only a one-way keyed hash is stored, so it cannot
                  be recovered.
                </span>
                {copyBlocked && (
                  <p role="alert" className="form-note" style={{ margin: 0 }}>
                    The browser blocked clipboard access on this origin — select the key and copy it with Ctrl/Cmd+C.
                  </p>
                )}
              </div>
              <p className="hint">
                Use it as the client credential: <code>Authorization: Bearer …</code> on the OpenAI surface,{' '}
                <code>x-api-key</code> on the Anthropic surface.
              </p>
            </div>
          )}
        </Modal>
      )}

      {confirmDelete !== null && (
        <Modal
          title="Delete API key"
          sub="This cannot be undone."
          onClose={() => setConfirmDelete(null)}
          footer={
            <>
              <button type="button" className="btn btn-secondary" onClick={() => setConfirmDelete(null)}>
                Cancel
              </button>
              <button type="button" className="btn btn-danger" onClick={() => void doDelete(confirmDelete)}>
                Delete key
              </button>
            </>
          }
        >
          <p>
            Delete <strong>{confirmDelete.name}</strong>? The secret stops working immediately and the row is removed
            from the database. Historical request logs keep the key id for attribution.
          </p>
        </Modal>
      )}

      {confirmRegenerate !== null && (
        <Modal
          title="Regenerate API key"
          sub="A new secret for the same key."
          onClose={() => setConfirmRegenerate(null)}
          footer={
            <>
              <button type="button" className="btn btn-secondary" onClick={() => setConfirmRegenerate(null)}>
                Cancel
              </button>
              <button
                type="button"
                className="btn btn-primary"
                onClick={() => void doRegenerate(confirmRegenerate)}
              >
                Regenerate
              </button>
            </>
          }
        >
          <p>
            Regenerate <strong>{confirmRegenerate.name}</strong>? The current secret stops working immediately. The new
            secret is shown once after you confirm.
          </p>
        </Modal>
      )}
    </>
  );
}
