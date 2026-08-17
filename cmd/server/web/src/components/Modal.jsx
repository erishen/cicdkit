import { useEffect } from 'react'

// Shared modal backdrop. While mounted it locks background page scroll.
// It does NOT close on a backdrop click — the caller must provide an explicit
// close control (the ✕ button in the header, or a footer button). Each caller
// keeps owning its own .modal-card content passed as children, so the
// scroll-lock logic lives in exactly one place and newly added modals can't
// forget to opt in (the previous approach tracked every open modal in App.jsx
// and was easy to miss).
export default function Modal({ onClose, className = '', children }) {
  useEffect(() => {
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => { document.body.style.overflow = prev }
  }, [])
  return (
    <div className={'modal ' + className}>
      {children}
    </div>
  )
}
