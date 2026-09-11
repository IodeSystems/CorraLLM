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
          <Typography variant="body2" sx={{
            color: "text.secondary"
          }}>
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
          {/* FOR THE PERSON WHO IS NOT THE OPERATOR.
              This page is everything a caller can reach: every /api/ route is
              gated on the admin token alone, so an API key gets 401 here and
              nowhere else to go. The screen said "admin token" three times and
              never the word they were actually given, leaving them to conclude
              they had the wrong key rather than the wrong door (Lenny, the
              handed-a-key scenario).
              It is deliberately the whole of the answer. A caller surface was
              considered and closed — callers use the API, not the dashboard —
              so this sentence is not a signpost to something else. */}
          <Typography variant="caption" sx={{ color: C.textMuted }}>
            Given an <strong>API key</strong> rather than an admin token? That key is for making
            requests, not for this page — it will not sign you in. Use it as your{' '}
            <Box component="code" sx={{ px: 0.5, bgcolor: C.canvas, borderRadius: 0.5 }}>
              Authorization: Bearer
            </Box>{' '}
            header against{' '}
            <Box component="code" sx={{ px: 0.5, bgcolor: C.canvas, borderRadius: 0.5 }}>
              /v1/chat/completions
            </Box>
            . The dashboard belongs to whoever runs the box; ask them for anything you need from
            it.
          </Typography>
          </Box>
        </Panel>
      </Box>
    </Box>
  );
}
