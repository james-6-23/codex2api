import { useToastContext } from '../components/ToastProvider'

// 兼容旧调用方：`const { toast, showToast } = useToast()` 不变。
// timeoutMs 参数已废弃 —— 全局 ToastProvider 用统一默认时长；
// 如需单次自定义时长，直接 `showToast(msg, type, ms)`。
export function useToast(_timeoutMs?: number) {
  void _timeoutMs
  return useToastContext()
}
