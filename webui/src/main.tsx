import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import App from './App';
import { AuthProvider } from './components/AuthContext';
import './styles/kit.css';
import './styles/app.css';

// Token layer first, kit layer second, webui-local additions third (K-1):
// kit.css @imports colors_and_type.css; app.css holds the few SPA-only
// layouts (login page, loading state) in terms of the same tokens.

// AuthProvider wraps everything below it: every route reaches the
// session state through useAuth (RequireAuth, Login, Shell, the data
// hooks) — without the provider mounted, useAuth throws and React
// unmounts the whole tree into an empty #root.

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <AuthProvider>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </AuthProvider>
  </StrictMode>,
);