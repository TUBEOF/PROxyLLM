import { useCallback, useEffect, useState } from 'react';
import { NavLink, Outlet, useLocation } from 'react-router-dom';
import { useAuth } from './AuthContext';
import {
  IconGauge,
  IconKey,
  IconList,
  IconLogout,
  IconPanelLeft,
  IconPlug,
  IconRoute,
  IconBoxes,
  IconSliders,
} from '../icons';

// App shell (kit "APP SHELL" section): sticky sidebar + topbar + routed
// content. The `collapsed` class is the kit's single lever — desktop shows
// the 64px icon rail, mobile slides the off-canvas sidebar in. Navigating
// closes it again on mobile so the content is never covered for long.
//
// The avatar shows a generic glyph, not an initial: there is no /me
// endpoint on the admin surface, so after a page refresh no username is
// known — fabricating one would violate the honest-UI rule.

const NAV_SECTIONS: { label: string; items: { to: string; name: string; icon: typeof IconGauge; end?: boolean }[] }[] = [
  {
    label: 'Monitor',
    items: [
      { to: '/', name: 'Overview', icon: IconGauge, end: true },
      { to: '/logs', name: 'Logs', icon: IconList },
    ],
  },
  {
    label: 'Configuration',
    items: [
      { to: '/providers', name: 'Providers', icon: IconPlug },
      { to: '/models', name: 'Models', icon: IconBoxes },
      { to: '/routing', name: 'Routing', icon: IconRoute },
    ],
  },
  {
    label: 'Access',
    items: [
      { to: '/keys', name: 'Client keys', icon: IconKey },
      { to: '/settings', name: 'Settings', icon: IconSliders },
    ],
  },
];

export default function Shell() {
  const { signOut } = useAuth();
  const [collapsed, setCollapsed] = useState(false);
  const location = useLocation();

  // After navigating, put the mobile off-canvas sidebar away again.
  useEffect(() => {
    setCollapsed(false);
  }, [location.pathname]);

  const toggleCollapsed = useCallback(() => {
    setCollapsed((c) => !c);
  }, []);

  return (
    <div className="appshell">
      <aside className={`sidebar${collapsed ? ' collapsed' : ''}`}>
        <a className="sidebar-brand" href="/">
          <span className="dot" />
          <span className="name">PROxyLLM</span>
        </a>
        <nav className="sidebar-nav">
          {NAV_SECTIONS.map((section) => (
            <div key={section.label}>
              <div className="sidebar-section">
                <span>{section.label}</span>
              </div>
              {section.items.map((item) => (
                <NavLink
                  key={item.to}
                  to={item.to}
                  end={item.end}
                  className={({ isActive }) => `sidebar-link${isActive ? ' active' : ''}`}
                >
                  <span className="ic">
                    <item.icon width="20" height="20" />
                  </span>
                  <span>{item.name}</span>
                </NavLink>
              ))}
            </div>
          ))}
        </nav>
        <div className="sidebar-foot">
          <button type="button" className="sidebar-link" onClick={() => void signOut()}>
            <span className="ic">
              <IconLogout width="20" height="20" />
            </span>
            <span>Sign out</span>
          </button>
        </div>
      </aside>

      <div className="content">
        <header className="topbar">
          <button
            type="button"
            className="collapse-toggle"
            onClick={toggleCollapsed}
            aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
          >
            <IconPanelLeft width="18" height="18" />
          </button>
          <div className="topbar-actions">
            <span className="avatar" title="Admin session">
              <svg viewBox="0 0 24 24" width="18" height="18" fill="currentColor" aria-hidden="true">
                <path d="M12 12a4 4 0 1 0-4-4 4 4 0 0 0 4 4Zm0 2c-4 0-8 2-8 5v1h16v-1c0-3-4-5-8-5Z" />
              </svg>
            </span>
          </div>
        </header>
        <Outlet />
      </div>
    </div>
  );
}