import { Suspense, lazy } from 'react';
import { Navigate, Route, Routes } from 'react-router-dom';
import Shell from './components/Shell';
import RequireAuth from './components/RequireAuth';
import Loading from './components/Loading';

// Route-level code splitting per research B-2: each §9 route is its own
// chunk loaded on navigation; the vendor runtime rides in one chunk.
const Login = lazy(() => import('./routes/Login'));
const Overview = lazy(() => import('./routes/Overview'));
const Providers = lazy(() => import('./routes/Providers'));
const ProviderType = lazy(() => import('./routes/ProviderType'));
const Models = lazy(() => import('./routes/Models'));
const Routing = lazy(() => import('./routes/Routing'));
const Logs = lazy(() => import('./routes/Logs'));
const Keys = lazy(() => import('./routes/Keys'));
const Settings = lazy(() => import('./routes/Settings'));

export default function App() {
  return (
    <Suspense fallback={<Loading label="Loading…" />}>
      <Routes>
        <Route path="/login" element={<Login />} />
        <Route
          path="/"
          element={
            <RequireAuth>
              <Shell />
            </RequireAuth>
          }
        >
          <Route index element={<Overview />} />
          <Route path="providers" element={<Providers />} />
          <Route path="providers/:type" element={<ProviderType />} />
          <Route path="models" element={<Models />} />
          <Route path="routing" element={<Routing />} />
          <Route path="logs" element={<Logs />} />
          <Route path="keys" element={<Keys />} />
          <Route path="settings" element={<Settings />} />
        </Route>
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </Suspense>
  );
}