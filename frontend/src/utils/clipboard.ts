/**
 * 剪贴板工具：统一 copyText 例程（navigator.clipboard 优先，textarea/execCommand 降级）。
 * 失败时抛不带 message 的 Error：调用方多经 extractErrorMessage(error, t(fallback)) 展示，
 * 空 message 可让本地化 fallback 文案生效，避免硬编码英文漏出。
 */

export const copyText = async (value: string): Promise<void> => {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(value)
    return
  }

  const textArea = document.createElement('textarea')
  textArea.value = value
  textArea.style.position = 'fixed'
  textArea.style.opacity = '0'
  document.body.appendChild(textArea)
  textArea.focus()
  textArea.select()
  const success = document.execCommand('copy')
  document.body.removeChild(textArea)
  if (!success) {
    // 不带 message，让调用方的本地化 fallback 文案生效
    throw new Error()
  }
}
