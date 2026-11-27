import type { CSSProperties } from 'react'
import ChannelLogo from './ChannelLogo'
import type { UpstreamChannel } from '../types'

interface ChannelNodeSpec {
  id: UpstreamChannel
  label: string
  /** Position in the 560x212 SVG coordinate space */
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

const VIEWBOX_W = 560
const VIEWBOX_H = 212

const CHANNEL_NODES: ChannelNodeSpec[] = [
  {
    id: 'codex',
    label: 'Codex',
    x: 68,
    y: 36,
    pinX: 98,
    pinY: 36,
    path: 'M 98 36 C 168 36, 174 88, 224 88',
    accent: '#6366F1',
    accentSoft: 'rgba(99, 102, 241, 0.24)',
    floatDelay: '0s',
    pulseDelay: '0s',
    pulseDuration: '3.2s',
  },
  {
    id: 'claude',
    label: 'Claude',
    x: 38,
    y: 106,
    pinX: 68,
    pinY: 106,
    path: 'M 68 106 L 220 106',
    accent: '#D97757',
    accentSoft: 'rgba(217, 119, 87, 0.24)',
    floatDelay: '-1.6s',
    pulseDelay: '0.9s',
    pulseDuration: '2.9s',
  },
  {
    id: 'antigravity',
    label: 'Antigravity',
    x: 68,
    y: 176,
    pinX: 98,
    pinY: 176,
    path: 'M 98 176 C 168 176, 174 124, 224 124',
    accent: '#0EA5E9',
    accentSoft: 'rgba(14, 165, 233, 0.24)',
    floatDelay: '-3.1s',
    pulseDelay: '1.8s',
    pulseDuration: '3.4s',
  },
  {
    id: 'opencode',
    label: 'OpenCode',
    x: 492,
    y: 36,
    pinX: 462,
    pinY: 36,
    path: 'M 462 36 C 392 36, 386 88, 336 88',
    accent: '#10B981',
    accentSoft: 'rgba(16, 185, 129, 0.24)',
    floatDelay: '-0.8s',
    pulseDelay: '0.45s',
    pulseDuration: '3.3s',
  },
  {
    id: 'grok',
    label: 'Grok',
    x: 522,
    y: 106,
    pinX: 492,
    pinY: 106,
    path: 'M 492 106 L 340 106',
    accent: '#64748B',
    accentSoft: 'rgba(148, 163, 184, 0.26)',
    floatDelay: '-2.3s',
    pulseDelay: '1.35s',
    pulseDuration: '3.0s',
  },
  {
    id: 'kiro',
    label: 'Kiro',
    x: 492,
    y: 176,
    pinX: 462,
    pinY: 176,
    path: 'M 462 176 C 392 176, 386 124, 336 124',
    accent: '#9046FF',
    accentSoft: 'rgba(144, 70, 255, 0.24)',
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
      className="relative mx-auto mb-5 aspect-[560/212] w-full max-w-[520px] select-none"
      aria-label={siteName}
    >
      {/* Subtle central aurora aura behind the gateway hub */}
      <div
        aria-hidden
        className="pointer-events-none absolute left-1/2 top-1/2 h-[58%] w-[36%] -translate-x-1/2 -translate-y-1/2 rounded-full bg-[radial-gradient(circle,rgba(56,189,248,0.22)_0%,rgba(168,85,247,0.16)_48%,transparent_76%)] blur-2xl dark:bg-[radial-gradient(circle,rgba(34,211,238,0.28)_0%,rgba(168,85,247,0.22)_50%,transparent_78%)]"
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
            const isLeft = node.x < VIEWBOX_W / 2
            return (
              <linearGradient
                key={`grad-${node.id}`}
                id={`auth-route-grad-${node.id}`}
                gradientUnits="userSpaceOnUse"
                x1={node.pinX}
                y1={node.pinY}
                x2={isLeft ? 224 : 336}
                y2={106}
              >
                <stop offset="0%" stopColor={node.accent} stopOpacity="0.72" />
                <stop offset="60%" stopColor="#38BDF8" stopOpacity="0.78" />
                <stop offset="100%" stopColor="#8B5CF6" stopOpacity="0.9" />
              </linearGradient>
            )
          })}

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
                fillOpacity="0.88"
              />
            </marker>
          ))}

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
              strokeOpacity="0.45"
              markerEnd={`url(#auth-route-arrow-${node.id})`}
            />

            {/* Animated luminous flowing dash along the route */}
            <path
              d={node.path}
              stroke={`url(#auth-route-grad-${node.id})`}
              strokeWidth="1.8"
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
              fillOpacity="0.85"
            />
            <circle
              cx={node.pinX}
              cy={node.pinY}
              r="5.5"
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
        </div>
      ))}

      {/* Centered Site Logo 3D Frosted Glass Hub Card */}
      <div className="absolute left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2">
        <div className="auth-glass-hub relative flex size-[74px] items-center justify-center rounded-[22px] p-2 sm:size-[88px] sm:rounded-[26px] sm:p-2.5">
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
