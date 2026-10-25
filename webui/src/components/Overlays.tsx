import { useEffect, useId, useRef } from 'react';
import type { ReactNode, RefObject } from 'react';
import { IconClose } from '../icons';

// Overlay wrappers around the kit's drawer/modal shells: scroll lock, focus
// trap, Escape and focus restoration in one place so every route gets
// identical behavior. The scroll lock is counted, so a confirm dialog opened
// from a drawer does not unlock the page underneath.

let openOverlays = 0;

const FOCUSABLE =
  'a[href], button:not([disabled]), textarea:not([disabled]), input:not([disabled]), ' +
  'select:not([disabled]), [tabindex]:not([tabindex="-1"])';

function useOverlayA11y(onClose: () => void, containerRef: RefObject<HTMLElement | null>) {
  useEffect(() => {
    const container = containerRef.current;
    const previous = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    openOverlays += 1;
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';

    const focusable = (): HTMLElement[] => {
      if (container === null) return [];
      return [...container.querySelectorAll<HTMLElement>(FOCUSABLE)].filter(
        (el) => el.offsetParent !== null || el === document.activeElement,
      );
    };
    const explicit = container?.querySelector<HTMLElement>('[data-autofocus]');
    if (explicit !== null && explicit !== undefined) explicit.focus();
    else {
      const first = focusable()[0];
      if (first !== undefined) first.focus();
      else container?.focus();
    }

    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        onClose();
        return;
      }
      if (e.key !== 'Tab' || container === null) return;
      const items = focusable();
      if (items.length === 0) {
        e.preventDefault();
        container.focus();
        return;
      }
      const head = items[0];
      const tail = items[items.length - 1];
      if (head === undefined || tail === undefined) return;
      if (e.shiftKey && document.activeElement === head) {
        e.preventDefault();
        tail.focus();
      } else if (!e.shiftKey && document.activeElement === tail) {
        e.preventDefault();
        head.focus();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => {
      window.removeEventListener('keydown', onKey);
      openOverlays = Math.max(0, openOverlays - 1);
      if (openOverlays === 0) document.body.style.overflow = prevOverflow;
      previous?.focus();
    };
  }, [onClose, containerRef]);
}

export function Drawer({
  title,
  onClose,
  children,
  footer,
  size = 'rail',
}: {
  title: string;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  // 'rail' is the 420px right-hand panel a form rides in. 'panel' is the
  // near-viewport popup a payload needs: a log's route trace and captured
  // bodies are JSON an operator reads, and 420px cuts it to slivers.
  size?: 'rail' | 'panel';
}) {
  const titleId = useId();
  const ref = useRef<HTMLElement>(null);
  useOverlayA11y(onClose, ref);
  return (
    <>
      <div className="drawer-backdrop" onClick={onClose} aria-hidden="true" />
      <aside
        ref={ref}
        className={size === 'panel' ? 'drawer drawer-panel' : 'drawer'}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
      >
        <div className="drawer-head">
          <h3 id={titleId}>{title}</h3>
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
  const titleId = useId();
  const ref = useRef<HTMLDivElement>(null);
  useOverlayA11y(onClose, ref);
  return (
    <>
      <div className="modal-backdrop" onClick={onClose} aria-hidden="true" />
      <div ref={ref} className="modal" role="dialog" aria-modal="true" aria-labelledby={titleId} tabIndex={-1}>
        <div className="modal-head">
          <div>
            <h3 id={titleId}>{title}</h3>
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

// ConfirmDialog is the shared destructive-action gate: it names the object,
// explains the consequence and defaults focus to Cancel.
export function ConfirmDialog({
  title,
  body,
  confirmLabel = 'Delete',
  onCancel,
  onConfirm,
  pending = false,
}: {
  title: string;
  body: string;
  confirmLabel?: string;
  onCancel: () => void;
  onConfirm: () => void;
  pending?: boolean;
}) {
  return (
    <Modal
      title={title}
      onClose={onCancel}
      footer={
        <>
          <button type="button" className="btn btn-ghost" onClick={onCancel} disabled={pending} data-autofocus>
            Cancel
          </button>
          <button type="button" className="btn btn-danger" onClick={onConfirm} disabled={pending}>
            {pending ? 'Working…' : confirmLabel}
          </button>
        </>
      }
    >
      <p className="sub">{body}</p>
    </Modal>
  );
}
