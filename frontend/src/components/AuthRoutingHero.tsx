import type { CSSProperties } from 'react'
import ChannelLogo from './ChannelLogo'
import type { UpstreamChannel } from '../types'

interface ChannelNodeSpec {
  id: UpstreamChannel
  label: string
  /** Position in the 580x236 SVG coordinate space */
  x: number
  y: number
  /** Cubic Bézier or line path from node edge to central hub edge */
  path: string
  /** Source pin coordinates at the node's inner edge */
  pinX: number
  pinY: number
  /** Accent hex for tinted glass glow, route gradient, and pulse */
  accent: string
  accentSoft: string
  floatDelay: string
  pulseDelay: string
  pulseDuration: string
}

const VIEWBOX_W = 580
const VIEWBOX_H = 236
const HUB_CX = 290
const HUB_CY = 108

const CHANNEL_NODES: ChannelNodeSpec[] = [
  {
    id: 'codex',
    label: 'Codex',
    x: 74,
    y: 34,
    pinX: 105,
    pinY: 34,
    path: 'M 105 34 C 176 34, 182 88, 232 88',
    accent: '#6366F1',
    accentSoft: 'rgba(99, 102, 241, 0.26)',
    floatDelay: '0s',
    pulseDelay: '0s',
    pulseDuration: '3.2s',
  },
  {
    id: 'claude',
    label: 'Claude',
    x: 42,
    y: 108,
    pinX: 73,
    pinY: 108,
    path: 'M 73 108 L 228 108',
    accent: '#D97757',
    accentSoft: 'rgba(217, 119, 87, 0.26)',
    floatDelay: '-1.6s',
    pulseDelay: '0.9s',
    pulseDuration: '2.9s',
  },
  {
    id: 'antigravity',
    label: 'Antigravity',
    x: 74,
    y: 182,
    pinX: 105,
    pinY: 182,
    path: 'M 105 182 C 176 182, 182 128, 232 128',
    accent: '#0EA5E9',
    accentSoft: 'rgba(14, 165, 233, 0.26)',
    floatDelay: '-3.1s',
    pulseDelay: '1.8s',
    pulseDuration: '3.4s',
  },
  {
    id: 'opencode',
    label: 'OpenCode',
    x: 506,
    y: 34,
    pinX: 475,
    pinY: 34,
    path: 'M 475 34 C 404 34, 398 88, 348 88',
    accent: '#10B981',
    accentSoft: 'rgba(16, 185, 129, 0.26)',
    floatDelay: '-0.8s',
    pulseDelay: '0.45s',
    pulseDuration: '3.3s',
  },
  {
    id: 'grok',
    label: 'Grok',
    x: 538,
    y: 108,
    pinX: 507,
    pinY: 108,
    path: 'M 507 108 L 352 108',
    accent: '#64748B',
    accentSoft: 'rgba(148, 163, 184, 0.28)',
    floatDelay: '-2.3s',
    pulseDelay: '1.35s',
    pulseDuration: '3.0s',
  },
  {
    id: 'kiro',
    label: 'Kiro',
    x: 506,
    y: 182,
    pinX: 475,
    pinY: 182,
    path: 'M 475 182 C 404 182, 398 128, 348 128',
    accent: '#9046FF',
    accentSoft: 'rgba(144, 70, 255, 0.26)',
    floatDelay: '-3.8s',
    pulseDelay: '2.2s',
    pulseDuration: '3.5s',
  },
]

interface AuthRoutingHeroProps {
  siteName: string
  logoSrc: string
}

export default function AuthRoutingHero({ siteName, logoSrc }: AuthRoutingHeroProps) {
  return (
    <div
      className="relative mx-auto mb-3 aspect-[580/236] w-full max-w-[540px] select-none"
      aria-label={siteName}
    >
      {/* Subtle 3-tone prism halo behind the centered gateway hub */}
      <div
        aria-hidden
        className="pointer-events-none absolute left-1/2 top-[45.8%] h-[56%] w-[36%] -translate-x-1/2 -translate-y-1/2 rounded-full bg-[radial-gradient(circle,rgba(56,189,248,0.24)_0%,rgba(52,211,153,0.14)_42%,rgba(168,85,247,0.16)_68%,transparent_82%)] blur-2xl dark:bg-[radial-gradient(circle,rgba(34,211,238,0.30)_0%,rgba(52,211,153,0.16)_44%,rgba(168,85,247,0.24)_70%,transparent_84%)]"
      />

      {/* Synchronized SVG routing arrows & animated energy pulses */}
      <svg
        aria-hidden
        viewBox={`0 0 ${VIEWBOX_W} ${VIEWBOX_H}`}
        fill="none"
        className="pointer-events-none absolute inset-0 h-full w-full overflow-visible"
      >
        <defs>
          {CHANNEL_NODES.map((node) => {
            const isLeft = node.x < HUB_CX
            return (
              <linearGradient
                key={`grad-${node.id}`}
                id={`auth-route-grad-${node.id}`}
                gradientUnits="userSpaceOnUse"
                x1={node.pinX}
                y1={node.pinY}
                x2={isLeft ? 232 : 348}
                y2={HUB_CY}
              >
                <stop offset="0%" stopColor={node.accent} stopOpacity="0.72" />
                <stop offset="56%" stopColor="#38BDF8" stopOpacity="0.8" />
                <stop offset="100%" stopColor="#8B5CF6" stopOpacity="0.92" />
              </linearGradient>
            )
          })}

          <linearGradient
            id="auth-route-stem"
            gradientUnits="userSpaceOnUse"
            x1={HUB_CX}
            y1={164}
            x2={HUB_CX}
            y2={232}
          >
            <stop offset="0%" stopColor="#38BDF8" stopOpacity="0.78" />
            <stop offset="55%" stopColor="#8B5CF6" stopOpacity="0.58" />
            <stop offset="100%" stopColor="#8B5CF6" stopOpacity="0.1" />
          </linearGradient>

          {CHANNEL_NODES.map((node) => (
            <marker
              key={`arrow-${node.id}`}
              id={`auth-route-arrow-${node.id}`}
              viewBox="0 0 10 10"
              refX="7.5"
              refY="5"
              markerWidth="6.5"
              markerHeight="6.5"
              orient="auto"
            >
              <path
                d="M 1.5 1.8 L 8.2 5 L 1.5 8.2 L 3.1 5 Z"
                fill="#6366F1"
                fillOpacity="0.9"
              />
            </marker>
          ))}

          <marker
            id="auth-stem-arrow"
            viewBox="0 0 10 10"
            refX="7.5"
            refY="5"
            markerWidth="6"
            markerHeight="6"
            orient="auto"
          >
            <path
              d="M 1.5 2 L 8 5 L 1.5 8 L 3.1 5 Z"
              fill="#8B5CF6"
              fillOpacity="0.65"
            />
          </marker>

          <filter id="auth-pulse-glow" x="-100%" y="-100%" width="300%" height="300%">
            <feGaussianBlur stdDeviation="2.4" result="blur" />
            <feMerge>
              <feMergeNode in="blur" />
              <feMergeNode in="SourceGraphic" />
            </feMerge>
          </filter>
        </defs>

        {CHANNEL_NODES.map((node) => (
          <g key={node.id}>
            {/* Base hairline route with crisp directional arrowhead */}
            <path
              d={node.path}
              stroke={`url(#auth-route-grad-${node.id})`}
              strokeWidth="1.65"
              strokeLinecap="round"
              strokeOpacity="0.46"
              markerEnd={`url(#auth-route-arrow-${node.id})`}
            />

            {/* Animated luminous flowing dash along the route */}
            <path
              d={node.path}
              stroke={`url(#auth-route-grad-${node.id})`}
              strokeWidth="1.85"
              strokeLinecap="round"
              strokeDasharray="6 14"
              className="auth-route-dash"
              style={{ animationDelay: node.floatDelay }}
            />

            {/* Source terminal pin at channel card exit */}
            <circle
              cx={node.pinX}
              cy={node.pinY}
              r="3"
              fill={node.accent}
              fillOpacity="0.88"
            />
            <circle
              cx={node.pinX}
              cy={node.pinY}
              r="5.8"
              fill={node.accent}
              fillOpacity="0.16"
            />

            {/* Traveling glowing packet orb */}
            <circle
              r="3.2"
              fill={node.accent}
              filter="url(#auth-pulse-glow)"
              className="auth-route-orb"
            >
              <animateMotion
                dur={node.pulseDuration}
                begin={node.pulseDelay}
                repeatCount="indefinite"
                path={node.path}
                keyPoints="0;1"
                keyTimes="0;1"
                calcMode="spline"
                keySplines="0.4 0 0.2 1"
              />
            </circle>
          </g>
        ))}

        {/* Downward unified gateway output stem linking hub -> title & login card */}
        <path
          d={`M ${HUB_CX} 166 L ${HUB_CX} 226`}
          stroke="url(#auth-route-stem)"
          strokeWidth="1.6"
          strokeLinecap="round"
          strokeDasharray="4 7"
          markerEnd="url(#auth-stem-arrow)"
          className="auth-route-dash"
        />
      </svg>

      {/* 6 Channel 3D Frosted Glass Mini Cards */}
      {CHANNEL_NODES.map((node) => (
        <div
          key={node.id}
          style={{
            left: `${(node.x / VIEWBOX_W) * 100}%`,
            top: `${(node.y / VIEWBOX_H) * 100}%`,
          }}
          className="absolute -translate-x-1/2 -translate-y-1/2"
        >
          <div className="relative flex items-center justify-center">
            <div
              title={node.label}
              aria-label={node.label}
              style={
                {
                  '--node-accent': node.accent,
                  '--node-glow': node.accentSoft,
                  animationDelay: node.floatDelay,
                } as CSSProperties
              }
              className="auth-glass-node group relative flex size-10 items-center justify-center rounded-[14px] sm:size-[50px] sm:rounded-[16px]"
            >
              <ChannelLogo
                channel={node.id}
                size={22}
                title={node.label}
                className="relative z-10 text-foreground/90 transition-transform duration-300 group-hover:scale-110 sm:scale-110"
              />
            </div>
            <span className="pointer-events-none absolute left-1/2 top-full mt-1.5 hidden -translate-x-1/2 whitespace-nowrap font-mono text-[9.5px] font-medium tracking-wide text-muted-foreground/75 sm:block">
              {node.label}
            </span>
          </div>
        </div>
      ))}

      {/* Centered Site Logo 3D Stacked Frosted Glass Hub Card */}
      <div
        style={{
          left: '50%',
          top: `${(HUB_CY / VIEWBOX_H) * 100}%`,
        }}
        className="absolute -translate-x-1/2 -translate-y-1/2"
      >
        {/* Subtle stacked glass echo plate underneath */}
        <div
          aria-hidden
          className="pointer-events-none absolute inset-x-2 -bottom-2 h-6 rounded-[20px] bg-gradient-to-r from-sky-400/25 via-emerald-400/20 to-violet-500/25 blur-sm dark:from-cyan-400/30 dark:via-emerald-400/22 dark:to-fuchsia-500/30"
        />
        <div className="auth-glass-hub relative flex size-[76px] items-center justify-center rounded-[22px] p-2 sm:size-[90px] sm:rounded-[26px] sm:p-2.5">
          <img
            src={logoSrc}
            alt={siteName}
            className="relative z-10 size-full rounded-[15px] object-cover shadow-sm sm:rounded-[18px]"
          />
        </div>
      </div>
    </div>
  )
}
