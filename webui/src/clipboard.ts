// Clipboard access with a fallback.
//
// navigator.clipboard exists only in a SECURE context (https or localhost).
// The console is commonly reached over plain HTTP at a LAN address, and on
// that origin the API is simply undefined: the old call threw a TypeError, the
// catch swallowed it, and the button kept saying "Copy" — a silent no-op the
// operator could not tell from a working button. The textarea path covers that
// origin; when even it fails the caller is told, so the UI can say so instead
// of pretending the text was copied.

export type CopyOutcome = 'copied' | 'failed';

export async function copyText(text: string): Promise<CopyOutcome> {
  if (typeof navigator !== 'undefined' && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return 'copied';
    } catch {
      // Permission denied or a transient failure — fall through to the
      // selection path rather than reporting a copy that did not happen.
    }
  }
  try {
    const area = document.createElement('textarea');
    area.value = text;
    area.setAttribute('readonly', '');
    area.style.position = 'fixed';
    area.style.top = '0';
    area.style.opacity = '0';
    document.body.appendChild(area);
    area.select();
    const ok = document.execCommand('copy');
    area.remove();
    return ok ? 'copied' : 'failed';
  } catch {
    return 'failed';
  }
}
