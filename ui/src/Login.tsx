import { useEffect, useState } from 'react'
import { Box, Button, TextField, Typography } from '@mui/material'
import { setToken } from './auth'
import { Panel } from '@/Panel'
import { C } from '@/theme'

// Login prompts for the admin token and points the operator at where it lives
// on the server. On submit it stores the token + cookie and reloads so the app
// starts authorized.
//
// The path is asked for rather than hardcoded: it follows --home, which now
// defaults to ~/.corrallm, so a fixed "home/admin.token" was wrong on every
// install that did not happen to run from the deployment directory. /health is
// unauthenticated (it has to be — the whole problem here is having no token) and
// reports the path only to callers on the server itself.
export function Login() {
  const [val, setVal] = useState('')
  const [tokenPath, setTokenPath] = useState<string | null>(null)
  useEffect(() => {
    fetch('/health')
      .then((r) => r.json())
      .then((d) => setTokenPath(typeof d?.tokenPath === 'string' ? d.tokenPath : null))
      .catch(() => setTokenPath(null))
  }, [])
  const submit = () => {
    const t = val.trim()
    if (!t) return
    setToken(t)
    window.location.reload()
  }
  return (
    <Box sx={{ display: 'flex', justifyContent: 'center', alignItems: 'center', minHeight: '100vh', p: 2 }}>
      <Box sx={{ maxWidth: 460, width: '100%' }}>
        <Panel title="corrallm — admin sign in">
          <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
          <Typography variant="body2" color="text.secondary">
            {tokenPath ? (
              <>
                This instance requires an admin token. Read it on the server and paste it below:{' '}
                <Box component="code" sx={{ px: 0.5, bgcolor: C.canvas, borderRadius: 0.5, wordBreak: 'break-all' }}>
                  cat {tokenPath}
                </Box>
              </>
            ) : (
              <>
                This instance requires an admin token. On the server it is the{' '}
                <Box component="code" sx={{ px: 0.5, bgcolor: C.canvas, borderRadius: 0.5 }}>
                  admin.token
                </Box>{' '}
                file in corrallm&rsquo;s home directory (
                <Box component="code" sx={{ px: 0.5, bgcolor: C.canvas, borderRadius: 0.5 }}>
                  ~/.corrallm
                </Box>{' '}
                by default; the exact path is printed in the startup log). Paste it below.
              </>
            )}
          </Typography>
          <TextField
            type="password"
            label="Admin token"
            value={val}
            onChange={(e) => setVal(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && submit()}
            fullWidth
            autoFocus
          />
          <Button variant="contained" onClick={submit} disabled={!val.trim()}>
            Sign in
          </Button>
          </Box>
        </Panel>
      </Box>
    </Box>
  )
}
