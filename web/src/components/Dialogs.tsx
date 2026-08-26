import { useEffect, useRef, type FormEvent, type ReactNode } from 'react'

interface DialogFrameProps {
  open: boolean
  title: string
  children: ReactNode
  pending?: boolean
  onClose: () => void
}

function DialogFrame({ open, title, children, pending = false, onClose }: DialogFrameProps) {
  useEffect(() => {
    if (!open) return
    function onKeyDown(event: KeyboardEvent) {
      if (event.key === 'Escape' && !pending) onClose()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [open, pending, onClose])

  if (!open) return null
  return (
    <div className="modal modal-open" role="dialog" aria-modal="true" aria-labelledby="app-dialog-title">
      <div className="modal-box max-w-md rounded-2xl border border-base-300 shadow-xl">
        <h3 id="app-dialog-title" className="text-lg font-semibold">{title}</h3>
        {children}
      </div>
      <button
        type="button"
        className="modal-backdrop cursor-default"
        aria-label="关闭弹框"
        disabled={pending}
        onClick={onClose}
      />
    </div>
  )
}

interface ConfirmDialogProps {
  open: boolean
  title: string
  description: ReactNode
  confirmLabel?: string
  cancelLabel?: string
  tone?: 'neutral' | 'warning' | 'danger'
  pending?: boolean
  onConfirm: () => void
  onClose: () => void
}

export function ConfirmDialog({
  open,
  title,
  description,
  confirmLabel = '确定',
  cancelLabel = '取消',
  tone = 'neutral',
  pending = false,
  onConfirm,
  onClose,
}: ConfirmDialogProps) {
  const confirmClass = tone === 'danger' ? 'btn-error' : tone === 'warning' ? 'btn-warning' : 'btn-neutral'
  return (
    <DialogFrame open={open} title={title} pending={pending} onClose={onClose}>
      <div className="mt-3 whitespace-pre-line text-sm leading-6 text-base-content/65">{description}</div>
      <div className="modal-action">
        <button type="button" className="btn btn-ghost" disabled={pending} onClick={onClose}>
          {cancelLabel}
        </button>
        <button type="button" className={`btn ${confirmClass}`} disabled={pending} onClick={onConfirm}>
          {pending && <span className="loading loading-spinner loading-sm" />}
          {pending ? '处理中...' : confirmLabel}
        </button>
      </div>
    </DialogFrame>
  )
}

interface TextInputDialogProps {
  open: boolean
  title: string
  label: string
  value: string
  placeholder?: string
  confirmLabel?: string
  pending?: boolean
  onChange: (value: string) => void
  onConfirm: () => void
  onClose: () => void
}

export function TextInputDialog({
  open,
  title,
  label,
  value,
  placeholder,
  confirmLabel = '确定',
  pending = false,
  onChange,
  onConfirm,
  onClose,
}: TextInputDialogProps) {
  const inputRef = useRef<HTMLInputElement>(null)
  useEffect(() => {
    if (!open) return
    requestAnimationFrame(() => inputRef.current?.focus())
  }, [open])

  function submit(event: FormEvent) {
    event.preventDefault()
    if (!pending) onConfirm()
  }

  return (
    <DialogFrame open={open} title={title} pending={pending} onClose={onClose}>
      <form onSubmit={submit} className="mt-5 grid gap-6">
        <label className="form-control gap-2">
          <span className="label-text">{label}</span>
          <input
            ref={inputRef}
            className="input input-bordered w-full"
            value={value}
            placeholder={placeholder}
            disabled={pending}
            onChange={(event) => onChange(event.target.value)}
          />
        </label>
        <div className="modal-action mt-0">
          <button type="button" className="btn btn-ghost" disabled={pending} onClick={onClose}>
            取消
          </button>
          <button type="submit" className="btn btn-neutral" disabled={pending}>
            {pending && <span className="loading loading-spinner loading-sm" />}
            {pending ? '处理中...' : confirmLabel}
          </button>
        </div>
      </form>
    </DialogFrame>
  )
}
