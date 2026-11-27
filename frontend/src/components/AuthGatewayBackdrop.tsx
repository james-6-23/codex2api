export default function AuthGatewayBackdrop() {
  return (
    <div aria-hidden className="pointer-events-none absolute inset-0 overflow-hidden select-none">
      {/* Harmonized 3-layer prism aurora blooms echoing the cyan / mint / violet logo */}
      <div className="absolute left-1/2 top-[22%] h-[420px] w-[680px] -translate-x-1/2 -translate-y-1/2 rounded-full bg-[radial-gradient(ellipse_at_center,rgba(56,189,248,0.13)_0%,rgba(52,211,153,0.07)_38%,rgba(168,85,247,0.08)_64%,transparent_80%)] blur-3xl dark:bg-[radial-gradient(ellipse_at_center,rgba(34,211,238,0.16)_0%,rgba(52,211,153,0.09)_38%,rgba(168,85,247,0.14)_66%,transparent_82%)]" />
      <div className="absolute -left-32 top-[18%] size-[420px] rounded-full bg-sky-400/[0.07] blur-3xl dark:bg-cyan-500/[0.08]" />
      <div className="absolute -right-28 bottom-[12%] size-[460px] rounded-full bg-violet-500/[0.07] blur-3xl dark:bg-fuchsia-500/[0.09]" />
      <div className="absolute bottom-[-120px] left-1/2 h-[280px] w-[560px] -translate-x-1/2 rounded-full bg-emerald-400/[0.05] blur-3xl dark:bg-emerald-500/[0.06]" />

      {/* Crisp fine architectural dot matrix (softly masked toward outer edges) */}
      <div className="absolute inset-0 opacity-[0.22] dark:opacity-[0.16] [background-image:radial-gradient(color-mix(in_oklab,var(--color-foreground)_52%,transparent)_1px,transparent_1px)] [background-size:28px_28px] [mask-image:radial-gradient(ellipse_72%_72%_at_50%_46%,#000_25%,transparent_100%)]" />

      {/* Original concentric orbital geometry & isometric prism light rails */}
      <svg
        viewBox="0 0 1440 900"
        preserveAspectRatio="xMidYMid slice"
        fill="none"
        className="absolute inset-0 h-full w-full"
      >
        <defs>
          <linearGradient id="auth-prism-ring-1" gradientUnits="userSpaceOnUse" x1="360" y1="140" x2="1080" y2="660">
            <stop offset="0%" stopColor="#38BDF8" stopOpacity="0.22" />
            <stop offset="50%" stopColor="#34D399" stopOpacity="0.18" />
            <stop offset="100%" stopColor="#A855F7" stopOpacity="0.24" />
          </linearGradient>
          <linearGradient id="auth-prism-ring-2" gradientUnits="userSpaceOnUse" x1="220" y1="680" x2="1220" y2="120">
            <stop offset="0%" stopColor="#A855F7" stopOpacity="0.16" />
            <stop offset="50%" stopColor="#38BDF8" stopOpacity="0.12" />
            <stop offset="100%" stopColor="#34D399" stopOpacity="0.18" />
          </linearGradient>
          <linearGradient id="auth-diag-rail" gradientUnits="userSpaceOnUse" x1="0" y1="0" x2="1440" y2="900">
            <stop offset="0%" stopColor="#38BDF8" stopOpacity="0" />
            <stop offset="32%" stopColor="#38BDF8" stopOpacity="0.14" />
            <stop offset="50%" stopColor="#34D399" stopOpacity="0.12" />
            <stop offset="68%" stopColor="#A855F7" stopOpacity="0.14" />
            <stop offset="100%" stopColor="#A855F7" stopOpacity="0" />
          </linearGradient>
        </defs>

        {/* Subtle isometric 45-degree diamond prism frame lines echoing the 3D stacked logo */}
        <g opacity="0.7">
          <path d="M 720 -40 L 1280 410 L 720 860 L 160 410 Z" stroke="url(#auth-diag-rail)" strokeWidth="1" />
          <path d="M 720 40 L 1180 410 L 720 780 L 260 410 Z" stroke="url(#auth-diag-rail)" strokeWidth="1" strokeDasharray="4 10" opacity="0.65" />
        </g>

        {/* Concentric orbital gateway arcs centered behind the hero */}
        <g transform="translate(720 320)">
          <circle r="210" stroke="url(#auth-prism-ring-1)" strokeWidth="1" strokeDasharray="3 9" opacity="0.85" />
          <circle r="335" stroke="url(#auth-prism-ring-1)" strokeWidth="1" opacity="0.55" />
          <circle r="470" stroke="url(#auth-prism-ring-2)" strokeWidth="1" strokeDasharray="6 14" opacity="0.5" />

          {/* Slow drifting orbital beacon nodes */}
          <g className="auth-orbit-slow" style={{ transformOrigin: '0px 0px' }}>
            <circle cx="335" cy="0" r="3" fill="#38BDF8" fillOpacity="0.72" />
            <circle cx="335" cy="0" r="8" fill="#38BDF8" fillOpacity="0.14" />
            <circle cx="-237" cy="237" r="2.6" fill="#A855F7" fillOpacity="0.68" />
            <circle cx="-237" cy="237" r="7" fill="#A855F7" fillOpacity="0.13" />
          </g>
          <g className="auth-orbit-reverse" style={{ transformOrigin: '0px 0px' }}>
            <circle cx="0" cy="-210" r="2.4" fill="#34D399" fillOpacity="0.72" />
            <circle cx="0" cy="-210" r="6.5" fill="#34D399" fillOpacity="0.14" />
          </g>
        </g>
      </svg>
    </div>
  )
}
