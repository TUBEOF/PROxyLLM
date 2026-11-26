import { useState } from 'react';
import { apiCall } from '../api';
import { Modal } from './Overlays';

// Versioned pre-connect warning (ADR-022). The acknowledgement is stored
// server-side (`risk_ack:<type>`), so it covers every admin and browser, and
// a notice-text change (version bump) re-opens this dialog.

export interface RiskNoticeModalProps {
  type: string;
  notice: string;
  version: number;
  onCancel: () => void;
  onContinue: () => void;
}

export default function RiskNoticeModal({ type, notice, version, onCancel, onContinue }: RiskNoticeModalProps) {
  const [remember, setRemember] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function proceed() {
    if (!remember) {
      onContinue();
      return;
    }
    setBusy(true);
    setError(null);
    try {
      await apiCall('POST', `/api/admin/provider-types/${encodeURIComponent(type)}/risk-ack`);
      onContinue();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not save the acknowledgement');
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      title="Before continuing"
      sub={`This provider type is shown with a warning (version ${version}).`}
      onClose={onCancel}
      footer={
        <>
          <button type="button" className="btn btn-secondary" onClick={onCancel} disabled={busy}>
            Cancel
          </button>
          <button type="button" className="btn btn-primary" onClick={() => void proceed()} disabled={busy}>
            {busy ? 'Saving…' : 'I understand, continue'}
          </button>
        </>
      }
    >
      <div className="form">
        <p className="risk-notice">{notice}</p>
        {error !== null && (
          <p role="alert" className="form-note">
            {error}
          </p>
        )}
        <label className="check">
          <input type="checkbox" checked={remember} onChange={(e) => setRemember(e.target.checked)} />
          <span className="cbox">
            <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3.5" aria-hidden="true">
              <path d="m5 13 4 4 10-10" />
            </svg>
          </span>
          <span>Don&apos;t show this again for this provider</span>
        </label>
        <p className="hint">
          The acknowledgement is stored on the server for all admins. Changing the warning text makes it appear
          again.
        </p>
      </div>
    </Modal>
  );
}
