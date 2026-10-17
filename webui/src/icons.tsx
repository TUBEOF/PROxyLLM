import type { ReactNode, SVGProps } from 'react';

// Inline SVG icon set (DESIGN.md: no emoji, no icon-font CDN — CSP stays
// script/style/img/font-src 'self'). Stroke-based, 24px viewBox, rendered
// at 20px to match .sidebar-link .ic and .btn .ic sizing.

interface IconProps extends SVGProps<SVGSVGElement> {
  children?: ReactNode;
}

function Icon({ children, ...props }: IconProps) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.8"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      {...props}
    >
      {children}
    </svg>
  );
}

export const IconGauge = (p: IconProps) => (
  <Icon {...p}>
    <path d="M12 14a2 2 0 1 0 0-4 2 2 0 0 0 0 4Z" />
    <path d="m13.4 12.6 4.6-4.6" />
    <path d="M12 4a9 9 0 0 1 9 9" />
    <path d="M3 13a9 9 0 0 1 9-9" opacity="0.4" />
    <path d="M3 13c0 2 .7 3.9 1.8 5.4" opacity="0.4" />
    <path d="M12 21c3.9 0 7.3-2.5 8.6-6" opacity="0.4" />
  </Icon>
);

export const IconPlug = (p: IconProps) => (
  <Icon {...p}>
    <path d="M9 3v6" />
    <path d="M15 3v6" />
    <path d="M7 9h10v3a5 5 0 0 1-10 0V9Z" />
    <path d="M12 17v4" />
  </Icon>
);

export const IconBoxes = (p: IconProps) => (
  <Icon {...p}>
    <path d="m12 3 8 4.5v9L12 21l-8-4.5v-9L12 3Z" />
    <path d="m4 7.5 8 4.5 8-4.5" />
    <path d="M12 12v9" />
  </Icon>
);

export const IconRoute = (p: IconProps) => (
  <Icon {...p}>
    <circle cx="5" cy="6" r="2.5" />
    <circle cx="19" cy="18" r="2.5" />
    <path d="M7.5 6h5a4 4 0 0 1 4 4v0a4 4 0 0 0 0 2" opacity="0.4" />
    <path d="M7.5 6H14a4 4 0 0 1 4 4v4a4 4 0 0 1-4 4h-4.5" />
  </Icon>
);

export const IconList = (p: IconProps) => (
  <Icon {...p}>
    <path d="M4 6h16" />
    <path d="M4 12h16" />
    <path d="M4 18h10" />
  </Icon>
);

export const IconKey = (p: IconProps) => (
  <Icon {...p}>
    <circle cx="8" cy="15" r="4.5" />
    <path d="m11.5 11.5 8-8" />
    <path d="m16 5 3 3" />
    <path d="m19.5 8.5 1.5-1.5" opacity="0.4" />
  </Icon>
);

export const IconSliders = (p: IconProps) => (
  <Icon {...p} >
    <path d="M4 7h9" />
    <path d="M17 7h3" />
    <circle cx="15" cy="7" r="2" />
    <path d="M4 17h3" />
    <path d="M11 17h9" />
    <circle cx="9" cy="17" r="2" />
  </Icon>
);

export const IconSearch = (p: IconProps) => (
  <Icon {...p}>
    <circle cx="11" cy="11" r="6.5" />
    <path d="m16 16 4.5 4.5" />
  </Icon>
);

export const IconMenu = (p: IconProps) => (
  <Icon {...p}>
    <path d="M4 6h16" />
    <path d="M4 12h16" />
    <path d="M4 18h16" />
  </Icon>
);

export const IconPanelLeft = (p: IconProps) => (
  <Icon {...p}>
    <rect x="3" y="4" width="18" height="16" rx="2.5" />
    <path d="M9.5 4v16" />
  </Icon>
);

export const IconLogout = (p: IconProps) => (
  <Icon {...p}>
    <path d="M14 4h4a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2h-4" />
    <path d="M10 8 6 12l4 4" />
    <path d="M6 12h9" />
  </Icon>
);

export const IconPlus = (p: IconProps) => (
  <Icon {...p}>
    <path d="M12 5v14" />
    <path d="M5 12h14" />
  </Icon>
);

export const IconClose = (p: IconProps) => (
  <Icon {...p}>
    <path d="m6 6 12 12" />
    <path d="m18 6-12 12" />
  </Icon>
);

export const IconChevronLeft = (p: IconProps) => (
  <Icon {...p}>
    <path d="m14 6-6 6 6 6" />
  </Icon>
);

export const IconChevronRight = (p: IconProps) => (
  <Icon {...p}>
    <path d="m10 6 6 6-6 6" />
  </Icon>
);

export const IconCopy = (p: IconProps) => (
  <Icon {...p}>
    <rect x="9" y="9" width="11" height="11" rx="2" />
    <path d="M5 15V6a2 2 0 0 1 2-2h9" />
  </Icon>
);

export const IconCheck = (p: IconProps) => (
  <Icon {...p}>
    <path d="m5 13 4 4 10-10" />
  </Icon>
);

export const IconAlert = (p: IconProps) => (
  <Icon {...p}>
    <path d="M12 4 2.5 20h19L12 4Z" />
    <path d="M12 10v4" />
    <path d="M12 17.5v.5" />
  </Icon>
);

export const IconRefresh = (p: IconProps) => (
  <Icon {...p}>
    <path d="M20 12a8 8 0 1 1-2.34-5.66" />
    <path d="M20 4v4h-4" />
  </Icon>
);

export const IconClock = (p: IconProps) => (
  <Icon {...p}>
    <circle cx="12" cy="12" r="9" />
    <path d="M12 7v5l3 2" />
  </Icon>
);

export const IconDelta = (p: IconProps) => (
  <Icon {...p}>
    <path d="M7 5v10" />
    <path d="M2 10h10" />
    <path d="M15 16h6" />
  </Icon>
);

export const IconExternal = (p: IconProps) => (
  <Icon {...p}>
    <path d="M14 4h6v6" />
    <path d="M20 4 10 14" />
    <path d="M18 14v4a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h4" />
  </Icon>
);