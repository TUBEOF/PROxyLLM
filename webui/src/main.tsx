import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import App from './App';
import './styles/kit.css';
import './styles/app.css';

// Token layer first, kit layer second, webui-local additions third (K-1):
// kit.css @imports colors_and_type.css; app.css holds the few SPA-only
// layouts (login page, loading state) in terms of the same tokens.

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </StrictMode>,
);