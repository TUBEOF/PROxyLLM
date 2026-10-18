import { useCallback, useEffect, useState } from 'react';
import { NavLink, Outlet, useLocation } from 'react-router-dom';
import UserMenu from './UserMenu';
import {
  IconGauge,
  IconKey,
  IconList,
  IconPanelLeft,
  IconPlug,
  IconRoute,
  IconBoxes,
} from '../icons';

// App shell (kit "APP SHELL" section): sticky sidebar + topbar + routed
// content. The `collapsed` class is the kit's single lever — desktop shows
// the 64px icon rail, mobile slides the off-canvas sidebar in. Navigating
// closes it again on mobile so the content is never covered for long.
//
// Account surface: the topbar avatar IS the user menu —
// identity, account settings (password change), sign out. The sidebar's
// bottom Sign out button is gone (it read as a nav item and broke the
// sections' visual line).

const NAV_SECTIONS: { label: string; items: { to: string; name: string; icon: typeof IconGauge; end?: boolean }[] }[] = [
  {
    label: 'Monitor',
    items: [
      { to: '/', name: 'Overview', icon: IconGauge, end: true },
      { to: '/quota', name: 'Quota', icon: IconPlug },
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
      { to: '/keys', name: 'API keys', icon: IconKey },
    ],
  },
];

export default function Shell() {
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
            <UserMenu />
          </div>
        </header>
        <Outlet />
      </div>
    </div>
  );
}
