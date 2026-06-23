import { useState } from 'react'
import { Box, Button, Card, CardContent, TextField, Typography } from '@mui/material'
import { setToken } from './auth'

// Login prompts for the admin token and points the operator at where it lives
// on the server (home/admin.token). On submit it stores the token + cookie and
// reloads so the app starts authorized.
export function Login() {
  const [val, setVal] = useState('')
  const submit = () => {
    const t = val.trim()
    if (!t) return
    setToken(t)
    window.location.reload()
  }
  return (
    <Box sx={{ display: 'flex', justifyContent: 'center', alignItems: 'center', minHeight: '100vh', p: 2 }}>
      <Card sx={{ maxWidth: 460, width: '100%' }}>
        <CardContent sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
          <Typography variant="h6">corrallm — admin sign in</Typography>
          <Typography variant="body2" color="text.secondary">
            This instance requires an admin token. On the server, read it from{' '}
            <Box component="code" sx={{ px: 0.5, bgcolor: 'grey.100', borderRadius: 0.5 }}>
              home/admin.token
            </Box>{' '}
            (e.g. <Box component="code" sx={{ px: 0.5, bgcolor: 'grey.100', borderRadius: 0.5 }}>cat home/admin.token</Box>) and paste it below.
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
        </CardContent>
      </Card>
    </Box>
  )
}
