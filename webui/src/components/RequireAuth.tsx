import type { ReactNode } from 'react';
import { Navigate, useLocation } from 'react-router-dom';
import { useAuth } from './AuthContext';

// Gate for every admin surface route: unauthenticated visitors land on
// /login. The optimistic `authenticated` start (AuthContext) means a
// redirect can happen late — the first admin call 401s → onSessionLost
// flips the flag → this gate re-renders to /login. That is the designed
// session-expiry path; no extra probe endpoint is needed (S-2).
export default function RequireAuth({ children }: { children: ReactNode }) {
  const { authenticated } = useAuth();
  const location = useLocation();

  if (!authenticated) {
    return <Navigate to="/login" replace state={{ from: location }} />;
  }
  return <>{children}</>;
}