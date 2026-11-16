import { useEffect, useState } from 'react'
import { api, PROXY_POOL_CHANGED, type ProxyRow } from '../api'

// Account pages share one invalidation rule; late requests must not restore old counts.
export function useProxyPool(enabled = true): ProxyRow[] {
  const [proxies, setProxies] = useState<ProxyRow[]>([])
  useEffect(() => {
    let sequence = 0
    let disposed = false
    if (!enabled) { setProxies([]); return }
    const reload = async () => {
      const current = ++sequence
      try {
        const result = await api.listProxies()
        if (!disposed && current === sequence) setProxies(result.proxies ?? [])
      } catch {
        if (!disposed && current === sequence) setProxies([])
      }
    }
    void reload()
    window.addEventListener(PROXY_POOL_CHANGED, reload)
    window.addEventListener('focus', reload)
    return () => {
      disposed = true
      window.removeEventListener(PROXY_POOL_CHANGED, reload)
      window.removeEventListener('focus', reload)
    }
  }, [enabled])
  return proxies
}
