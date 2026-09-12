package codex

// ReLoginRequiredError signals that the stored OAuth identity can no longer
// be refreshed — the single-use refresh token was consumed elsewhere,
// expired, or was revoked (X11) — so the operator must run a fresh login.
// It is an operator action, never a silent retry loop (X12).
type ReLoginRequiredError struct {
	Reason string
}

func (e *ReLoginRequiredError) Error() string {
	return "codex: re-login required: " + e.Reason
}

// DeviceLoginDisabledError signals that the issuer answered the initial
// device-code request with 404 (X20: device login disabled for this client).
// An operator-facing error, not retried.
type DeviceLoginDisabledError struct {
	Detail string
}

func (e *DeviceLoginDisabledError) Error() string {
	return "codex: device login disabled: " + e.Detail
}
