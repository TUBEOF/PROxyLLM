import { useCallback, useEffect, useState } from 'react';
import { Link, NavLink, Outlet, useLocation } from 'react-router-dom';
import UserMenu from './UserMenu';
import {
  IconBarChart,
  IconGauge,
  IconKey,
  IconList,
  IconPanelLeft,
  IconPlug,
  IconRoute,
  IconBoxes,
} from '../icons';

// App shell (kit "APP SHELL" section): sticky sidebar + topbar + routed
// content. Desktop `collapsed` is the 64px icon rail and persists in
// localStorage; mobile `mobile-open` slides the off-canvas sidebar in and is
// closed on navigation. The two states are separate so changing a desktop
// preference never opens the phone drawer.
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
      { to: '/quota', name: 'Quota', icon: IconBarChart },
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

function readCollapsed(): boolean {
  try {
    return localStorage.getItem('proxyllm.sidebar') === 'collapsed';
  } catch {
    return false;
  }
}

export default function Shell() {
  const [collapsed, setCollapsed] = useState(readCollapsed);
  const [mobileOpen, setMobileOpen] = useState(false);
  const [mobile, setMobile] = useState(
    () => typeof window !== 'undefined' && window.matchMedia('(max-width: 768px)').matches,
  );
  const location = useLocation();

  useEffect(() => {
    const query = window.matchMedia('(max-width: 768px)');
    const onChange = () => setMobile(query.matches);
    query.addEventListener('change', onChange);
    return () => query.removeEventListener('change', onChange);
  }, []);

  // Navigating closes the mobile drawer; a desktop collapse preference stays.
  useEffect(() => {
    setMobileOpen(false);
  }, [location.pathname]);

  const toggleCollapsed = useCallback(() => {
    if (mobile) {
      setMobileOpen((open) => !open);
      return;
    }
    setCollapsed((c) => {
      const next = !c;
      try {
        localStorage.setItem('proxyllm.sidebar', next ? 'collapsed' : 'expanded');
      } catch {
        // Storage can be unavailable (private mode); the rail still toggles.
      }
      return next;
    });
  }, [mobile]);

  const expanded = mobile ? mobileOpen : !collapsed;
  return (
    <div className="appshell">
      <aside
        id="app-sidebar"
        className={`sidebar${collapsed ? ' collapsed' : ''}${mobileOpen ? ' mobile-open' : ''}`}
      >
        <Link className="sidebar-brand" to="/" aria-label="PROxyLLM home">
          <span className="dot" />
          <span className="name">PROxyLLM</span>
        </Link>
        <nav className="sidebar-nav" aria-label="Main navigation">
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
                  title={item.name}
                  aria-label={item.name}
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
            aria-label={expanded ? 'Collapse sidebar' : 'Expand sidebar'}
            aria-expanded={expanded}
            aria-controls="app-sidebar"
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
