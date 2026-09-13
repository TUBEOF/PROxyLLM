import { useEffect } from 'react';
import type { ReactNode } from 'react';
import { IconClose } from '../icons';

// Overlay wrappers around the kit's drawer/modal shells: focus + Escape
// handling in one place so every route gets identical behavior.

// Lock scroll while an overlay is open, and close on Escape.
function useOverlayA11y(onClose: () => void, open: boolean) {
  useEffect(() => {
    if (!open) return;
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => {
      document.body.style.overflow = prevOverflow;
      window.removeEventListener('keydown', onKey);
    };
  }, [onClose, open]);
}

export function Drawer({
  title,
  onClose,
  children,
  footer,
}: {
  title: string;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
}) {
  useOverlayA11y(onClose, true);
  return (
    <>
      <div className="drawer-backdrop" onClick={onClose} aria-hidden="true" />
      <aside className="drawer" role="dialog" aria-modal="true" aria-label={title}>
        <div className="drawer-head">
          <h3>{title}</h3>
          <button type="button" className="btn btn-icon btn-ghost" onClick={onClose} aria-label="Close">
            <IconClose width="16" height="16" />
          </button>
        </div>
        <div className="drawer-body">{children}</div>
        {footer !== undefined && <div className="drawer-foot">{footer}</div>}
      </aside>
    </>
  );
}

export function Modal({
  title,
  sub,
  onClose,
  children,
  footer,
}: {
  title: string;
  sub?: string;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
}) {
  useOverlayA11y(onClose, true);
  return (
    <>
      <div className="modal-backdrop" onClick={onClose} aria-hidden="true" />
      <div className="modal" role="dialog" aria-modal="true" aria-label={title}>
        <div className="modal-head">
          <div>
            <h3>{title}</h3>
            {sub !== undefined && <p>{sub}</p>}
          </div>
          <button type="button" className="btn btn-icon btn-ghost" onClick={onClose} aria-label="Close">
            <IconClose width="16" height="16" />
          </button>
        </div>
        <div className="modal-body">{children}</div>
        {footer !== undefined && <div className="modal-foot">{footer}</div>}
      </div>
    </>
  );
}