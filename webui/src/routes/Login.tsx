import { FormEvent, useCallback, useState } from 'react';
import { Navigate, useLocation, useNavigate } from 'react-router-dom';
import { useAuth } from '../components/AuthContext';
import { ApiError } from '../api';
import { IconAlert } from '../icons';

// Login doubles as first-run setup: the SPA cannot know whether an admin
// exists (there is deliberately no probe endpoint), so both modes live on
// this page. The server is the arbiter either way:
// - "Create first admin" submits bootstrap → 409 means an admin already
//   exists (zero-admin window closed) → flip to sign-in mode with a note.
// - "Sign in" that fails with 401 stays a plain wrong-credentials error
//   (uniform bodies never reveal whether the username or password was wrong).

export default function Login() {
  const { signIn, createFirstAdmin, authenticated } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();

  const [setupMode, setSetupMode] = useState(false);
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const submit = useCallback(
    async (e: FormEvent) => {
      e.preventDefault();
      setPending(true);
      setError(null);
      setNotice(null);
      try {
        if (setupMode) {
          await createFirstAdmin(username, password);
        } else {
          await signIn(username, password);
        }
        // Back to where the user was headed (RequireAuth stashed `from`),
        // or the overview.
        const from = (location.state as { from?: { pathname?: string } } | null)?.from?.pathname;
        navigate(from && from !== '/login' ? from : '/', { replace: true });
      } catch (err) {
        if (setupMode && err instanceof ApiError && err.status === 409) {
          // Bootstrap refused: an admin exists. Switch to sign-in.
          setSetupMode(false);
          setNotice('An admin account already exists — sign in instead.');
        } else {
          setError(err instanceof Error ? err.message : 'Sign-in failed');
        }
      } finally {
        setPending(false);
      }
    },
    [setupMode, username, password, createFirstAdmin, signIn, navigate, location.state],
  );

  // A stale optimistic session (authenticated already true) needs no login
  // form at all — Navigate declaratively (navigate() during render is
  // illegal in React Router 6).
  if (authenticated) {
    return <Navigate to="/" replace />;
  }

  return (
    <div className="login-wrap">
      <form className="login-card" onSubmit={submit}>
        <h1>{setupMode ? 'Create first admin' : 'PROxyLLM Console'}</h1>
        <p className="sub">
          {setupMode
            ? 'No admin exists yet. This choice is available exactly once — the account you create here manages this proxy.'
            : 'Sign in with your admin account.'}
        </p>

        {notice !== null && (
          <div className="login-error" role="status">
            <IconAlert width="16" height="16" />
            <span>{notice}</span>
          </div>
        )}
        {error !== null && (
          <div className="login-error" role="alert">
            <IconAlert width="16" height="16" />
            <span>{error}</span>
          </div>
        )}

        <div className="form">
          <label className="field">
            <span>Username</span>
            <input
              className="input"
              type="text"
              autoComplete="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              required
              autoFocus
            />
          </label>
          <label className="field">
            <span>Password</span>
            <input
              className="input"
              type="password"
              autoComplete={setupMode ? 'new-password' : 'current-password'}
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
            />
          </label>
          <button className="btn btn-primary" type="submit" disabled={pending || username === '' || password === ''}>
            {pending ? 'Working…' : setupMode ? 'Create admin' : 'Sign in'}
          </button>
          <button
            type="button"
            className="btn btn-ghost"
            onClick={() => {
              setSetupMode((m) => !m);
              setError(null);
              setNotice(null);
            }}
          >
            {setupMode ? 'Sign in instead' : 'First run? Create admin'}
          </button>
        </div>
      </form>
    </div>
  );
}