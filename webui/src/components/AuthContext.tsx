import { createContext, useCallback, useContext, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { bootstrap, clearCsrfToken, login, logout as apiLogout } from '../api';

// Session state for the SPA: bootstrap on a fresh instance (first admin,
// once — the server refuses with 409 afterwards), login afterwards, logout
// kills the server session row + clears state. A 401 on any admin call
// flips `authenticated` off via onSessionLost → RequireAuth shows /login.
// The admin-exists probe is deliberately absent server-side, so
// "first-time setup" is a UI-local mode on the login page, enforced by
// the server either way (bootstrap 409s once an admin exists).

interface AuthState {
  authenticated: boolean;
  signIn: (username: string, password: string) => Promise<void>;
  createFirstAdmin: (username: string, password: string) => Promise<void>;
  signOut: () => Promise<void>;
  onSessionLost: () => void;
}

const AuthContext = createContext<AuthState | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  // Optimistic start: a session cookie may already exist from a previous
  // visit. The first admin call either works (session alive) or 401s —
  // which flips authenticated off via onSessionLost.
  const [authenticated, setAuthenticated] = useState(true);

  const signIn = useCallback(async (username: string, password: string) => {
    await login(username, password);
    setAuthenticated(true);
  }, []);

  const createFirstAdmin = useCallback(async (username: string, password: string) => {
    await bootstrap(username, password);
    setAuthenticated(true);
  }, []);

  const signOut = useCallback(async () => {
    await apiLogout();
    setAuthenticated(false);
  }, []);

  const onSessionLost = useCallback(() => {
    setAuthenticated(false);
    clearCsrfToken();
  }, []);

  const value = useMemo(
    () => ({ authenticated, signIn, createFirstAdmin, signOut, onSessionLost }),
    [authenticated, signIn, createFirstAdmin, signOut, onSessionLost],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (ctx === null) {
    throw new Error('useAuth must be used inside AuthProvider');
  }
  return ctx;
}