import type { ReactNode } from 'react'

interface StatCardProps {
  icon: ReactNode
  iconClass: string
  label: string
  value: number | string
  sub?: string
}

export default function StatCard({ icon, iconClass, label, value, sub }: StatCardProps) {
  return (
    <div className="card stat-card">
      <div className="stat-card-top">
        <div className="stat-info">
          <label>{label}</label>
          <div className="value">{value}</div>
        </div>
        <div className={`stat-icon ${iconClass}`} aria-hidden="true">
          {icon}
        </div>
      </div>
      {sub ? <div className="sub">{sub}</div> : null}
    </div>
  )
}
