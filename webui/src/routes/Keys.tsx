import { FormEvent, useCallback, useMemo, useState } from 'react';
import { apiCall } from '../api';
import type { ClientKey, CreatedKey, KeysResponse } from '../types';
import { EmptyState, ErrorNote, PageHead, useAdminData, useMutation } from '../hooks';
import { Modal } from '../components/Overlays';
import { IconCheck, IconCopy, IconPlus } from '../icons';

// Client keys (plan §9): create modal + revoke. The server returns the full
// key exactly once (Analysis G control 1: Argon2id hash + display prefix are
// the only persisted remnants) — this modal is the only place the secret is
// ever shown, with an explicit display-once warning and a copy button.

export default function Keys() {
  const { data, loading, error, reload } = useAdminData<KeysResponse>(
    (signal) => apiCall<KeysResponse>('GET', '/api/admin/keys', undefined, signal),
  );
  const keys = useMemo(() => data?.keys ?? [], [data]);

  const [creating, setCreating] = useState(false);
  const [name, setName] = useState('');
  const [formError, setFormError] = useState<string | null>(null);
  // The display-once secret + its row id, held only while the modal is open.
  const [created, setCreated] = useState<CreatedKey | null>(null);
  const [copied, setCopied] = useState(false);

  const create = useMutation();
  const revoke = useMutation();

  const openCreate = useCallback(() => {
    setName('');
    setFormError(null);
    setCreating(true);
  }, []);

  const closeCreate = useCallback(() => {
    // Closing after creation discards the secret — it cannot be shown again.
    setCreating(false);
    setCreated(null);
    setCopied(false);
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
        setCreated(result);
        reload();
      }
    },
    [name, create, reload],
  );

  const copy = useCallback(async () => {
    if (created === null) return;
    try {
      await navigator.clipboard.writeText(created.key);
      setCopied(true);
    } catch {
      // Clipboard access can fail (insecure context, permission); the key
      // stays selectable for manual copy instead of surfacing an error.
      setCopied(false);
    }
  }, [created]);

  const doRevoke = useCallback(
    async (k: ClientKey) => {
      const ok = await revoke.run(() => apiCall('POST', `/api/admin/keys/${encodeURIComponent(k.id)}/revoke`));
      if (ok) reload();
    },
    [revoke, reload],
  );

  return (
    <>
      <PageHead
        title="Client keys"
        sub="Keys clients use against both API surfaces. Secrets are shown once at creation and stored as Argon2id hashes only."
        actions={
          <button type="button" className="btn btn-primary" onClick={openCreate}>
            <IconPlus width="16" height="16" />
            Create key
          </button>
        }
      />
      <div className="canvas">
        {error !== null ? (
          <div className="w-full">
            <ErrorNote message={error} />
          </div>
        ) : loading ? (
          <div className="w-full">
            <EmptyState title="Loading client keys" body="Fetching the key list." />
          </div>
        ) : keys.length === 0 ? (
          <div className="w-full">
            <EmptyState
              title="No client keys"
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
                      <th>Status</th>
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
                        <td>
                          {k.revoked ? (
                            <span className="badge down">
                              <span className="b-dot" />
                              revoked
                            </span>
                          ) : (
                            <span className="badge up">
                              <span className="b-dot" />
                              active
                            </span>
                          )}
                        </td>
                        <td>
                          {!k.revoked && (
                            <span className="row-actions">
                              <button
                                type="button"
                                className="btn btn-danger btn-sm"
                                onClick={() => void doRevoke(k)}
                                disabled={revoke.pending}
                              >
                                Revoke
                              </button>
                            </span>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
            {revoke.error !== null && <ErrorNote message={revoke.error} />}
          </div>
        )}
      </div>

      {creating && (
        <Modal
          title={created === null ? 'Create client key' : 'Key created'}
          sub={created === null ? 'Name the key for the audit list.' : undefined}
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
                  <input className="input mono" type="text" value={created.key} readOnly onFocus={(e) => e.target.select()} />
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
                  This is the only time the full key is visible — it is stored as an Argon2id hash and cannot be recovered.
                </span>
              </div>
              <p className="hint">
                Use it as the client credential: `Authorization: Bearer …` on the OpenAI surface, `x-api-key` on the
                Anthropic surface.
              </p>
            </div>
          )}
        </Modal>
      )}
    </>
  );
}