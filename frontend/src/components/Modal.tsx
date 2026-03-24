import type { MouseEvent, ReactNode } from 'react'

interface ModalProps {
  show: boolean
  title: string
  onClose: () => void
  children: ReactNode
  footer?: ReactNode
}

export default function Modal({ show, title, onClose, children, footer }: ModalProps) {
  if (!show) {
    return null
  }

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div
        className="modal-content"
        onClick={(event: MouseEvent<HTMLDivElement>) => event.stopPropagation()}
      >
        <div className="modal-header">
          <h3>{title}</h3>
          <button className="btn-icon" onClick={onClose} aria-label="关闭弹窗">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round">
              <line x1="18" y1="6" x2="6" y2="18" />
              <line x1="6" y1="6" x2="18" y2="18" />
            </svg>
          </button>
        </div>
        <div className="modal-body">{children}</div>
        {footer ? <div className="modal-footer">{footer}</div> : null}
      </div>
    </div>
  )
}
