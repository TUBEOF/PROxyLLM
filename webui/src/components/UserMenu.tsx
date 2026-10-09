import { useEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { useAuth } from './AuthContext';
import { apiCall } from '../api';
import { Modal } from './Overlays';
import { IconLogout } from '../icons';

// UserMenu (2026-10-08): the topbar avatar becomes the account menu —
// username (from GET /api/admin/me, the honest one after a refresh),
// "Account settings" (the password change modal), "Sign out". The
// sidebar's bottom Sign out button is gone; the avatar is the single
// account surface. Outside-click + Escape close it; the menu is the
// kit's vocabulary (widget surface, btn rows).
//
// The menu renders through a PORTAL to document.body: the topbar carries
// a backdrop-filter, which makes it the containing block for any
// position:fixed descendant — the account modal inside it pinned to the
// topbar instead of centering in the viewport. Portal escapes that.
// The modal pair (backdrop + .modal) is a sibling of the menu and mounts
// wherever this component sits, so both portal out together.

export default function UserMenu() {
  const { signOut } = useAuth();
  const [open, setOpen] = useState(false);
  const [username, setUsername] = useState<string | null>(null);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement | null>(null);
  const menuRef = useRef<HTMLDivElement | null>(null);
  const avatarRef = useRef<HTMLButtonElement | null>(null);

  // The username is fetched when the menu first opens — no /me poll, no
  // fabricated name before then (the honest-UI rule: null renders the
  // generic glyph, never an invented initial).
  useEffect(() => {
    if (!open || username !== null) return;
    let cancelled = false;
    apiCall<{ username: string }>('GET', '/api/admin/me')
      .then((res) => {
        if (!cancelled) setUsername(res.username);
      })
      .catch(() => {
        // Session oddity: the menu still works (sign out); the name stays
        // unknown rather than fabricated.
      });
    return () => {
      cancelled = true;
    };
  }, [open, username]);

  useEffect(() => {
    if (!open) return;
    const onClick = (e: MouseEvent) => {
      const t = e.target as Node;
      // Inside the avatar wrapper OR inside the portaled menu — the menu
      // lives at document.body, so "outside the root" would kill it mid-
      // click (the item unmounts before the click event lands).
      if (rootRef.current !== null && rootRef.current.contains(t)) return;
      if (menuRef.current !== null && menuRef.current.contains(t)) return;
      setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false);
    };
    window.addEventListener('mousedown', onClick);
    window.addEventListener('keydown', onKey);
    return () => {
      window.removeEventListener('mousedown', onClick);
      window.removeEventListener('keydown', onKey);
    };
  }, [open]);

  // Live position: the menu re-anchors on resize/scroll while open (the
  // snapshot-at-open form detached it from the avatar on viewport changes).
  const [pos, setPos] = useState<{ top: number; left: number } | null>(null);
  useEffect(() => {
    if (!open) {
      setPos(null);
      return;
    }
    const measure = () => {
      const rect = avatarRef.current?.getBoundingClientRect();
      if (rect) setPos({ top: rect.bottom + 8, left: Math.max(rect.right - 210, 8) });
    };
    measure();
    window.addEventListener('resize', measure);
    window.addEventListener('scroll', measure, true);
    return () => {
      window.removeEventListener('resize', measure);
      window.removeEventListener('scroll', measure, true);
    };
  }, [open]);

  return (
    <div ref={rootRef} style={{ position: 'relative' }}>
      <button
        type="button"
        className="avatar"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label="Account menu"
        onClick={() => setOpen((o) => !o)}
        ref={avatarRef}
      >
        <svg viewBox="0 0 24 24" width="18" height="18" fill="currentColor" aria-hidden="true">
          <path d="M12 12a4 4 0 1 0-4-4 4 4 0 0 0 4 4Zm0 2c-4 0-8 2-8 5v1h16v-1c0-3-4-5-8-5Z" />
        </svg>
      </button>
      {open &&
        createPortal(
          <div
            ref={menuRef}
            className="user-menu"
            role="menu"
            aria-label="Account"
            style={
              pos
                ? { position: 'fixed', top: pos.top, left: pos.left }
                : { position: 'fixed', right: 24, top: 64 }
            }
          >
            <div className="user-menu-head">
              <span className="user-menu-name">{username ?? 'Admin'}</span>
              <span className="hint">signed in</span>
            </div>
            <button
              type="button"
              className="user-menu-item"
              role="menuitem"
              onClick={() => {
                setOpen(false);
                setSettingsOpen(true);
              }}
            >
              Account settings
            </button>
            <button
              type="button"
              className="user-menu-item user-menu-signout"
              role="menuitem"
              onClick={() => void signOut()}
            >
              <IconLogout width="15" height="15" />
              Sign out
            </button>
          </div>,
          document.body,
        )}
      {settingsOpen &&
        createPortal(
          <AccountSettings onClose={() => setSettingsOpen(false)} />,
          document.body,
        )}
    </div>
  );
}

// AccountSettings is the password-change modal: current + new + repeat,
// client-side equality check, server contract PUT /api/admin/password
// {"current_password","new_password"} — uniform 400 on a wrong current
// password or a too-short one (the server response never says which).
function AccountSettings({ onClose }: { onClose: () => void }) {
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [repeat, setRepeat] = useState('');
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState(false);

  const submit = async () => {
    if (next !== repeat) {
      setError('The new passwords do not match');
      return;
    }
    setPending(true);
    setError(null);
    try {
      await apiCall('PUT', '/api/admin/password', {
        current_password: current,
        new_password: next,
      });
      setDone(true);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Request failed');
    } finally {
      setPending(false);
    }
  };

  const canSubmit = current !== '' && next.length >= 8 && next === repeat && !pending;

  return (
    <Modal
      title="Account settings"
      sub="Change the admin password. Other signed-in sessions are signed out; this one stays."
      onClose={onClose}
      footer={
        <>
          <button type="button" className="btn btn-secondary" onClick={onClose}>
            {done ? 'Close' : 'Cancel'}
          </button>
          {!done && (
            <button type="submit" className="btn btn-primary" form="account-settings-form" disabled={!canSubmit}>
              Change password
            </button>
          )}
        </>
      }
    >
      {done ? (
        <p className="hint" style={{ margin: 0 }}>
          Password changed. Use the new password at the next sign-in.
        </p>
      ) : (
        <form className="form" id="account-settings-form" onSubmit={(e) => { e.preventDefault(); void submit(); }}>
          {error !== null && (
            <p role="alert" className="form-note">{error}</p>
          )}
          <div className="field">
            <label htmlFor="as-current">Current password</label>
            <input
              id="as-current"
              className="input"
              type="password"
              autoComplete="current-password"
              value={current}
              onChange={(e) => setCurrent(e.target.value)}
              required
            />
          </div>
          <div className="field">
            <label htmlFor="as-new">New password (min. 8 characters)</label>
            <input
              id="as-new"
              className="input"
              type="password"
              autoComplete="new-password"
              minLength={8}
              value={next}
              onChange={(e) => setNext(e.target.value)}
              required
            />
          </div>
          <div className="field">
            <label htmlFor="as-repeat">Repeat new password</label>
            <input
              id="as-repeat"
              className="input"
              type="password"
              autoComplete="new-password"
              value={repeat}
              onChange={(e) => setRepeat(e.target.value)}
              required
            />
          </div>
        </form>
      )}
    </Modal>
  );
}