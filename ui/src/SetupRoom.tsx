import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Box, Button, Stack, TextField, Typography } from '@mui/material'
import { graphql } from '@/gql'
import { gqlClient } from '@/gqlClient'
import { Panel } from '@/Panel'
import { C } from '@/theme'

const RoomDoc = graphql(/* GraphQL */ `
  query RoomSettings {
    corrallm {
      usageRollup(windowHours: 1) {
        thermal {
          mode
          ambientF
          coolTargetF
          coolCostPerTherm
          heatTargetF
          heatCostPerTherm
          notes
        }
      }
    }
  }
`)

const SetThermalDoc = graphql(/* GraphQL */ `
  mutation SetThermal($body: corrallm_SetThermalInputBodyInput!) {
    corrallm {
      setThermal(body: $body) {
        ok
        message
      }
    }
  }
`)

type Form = {
  ambientF: string
  coolTargetF: string
  coolCostPerTherm: string
  heatTargetF: string
  heatCostPerTherm: string
  notes: string
}

/**
 * THE ROOM THE BOX HEATS.
 *
 * Every watt-hour local work draws ends up as heat in this room. Whether that
 * heat costs money (the room is being cooled) or saves it (the room is being
 * heated) depends on the room's own climate system, which corrallm cannot see —
 * so it is stated here: the ambient temperature as you read it, the targets the
 * room is held at, and what a therm (100,000 BTU) of heat costs to remove or is
 * worth to add. Traffic turns the BTU it reports into dollars with these.
 */
export function RoomSettings() {
  const qc = useQueryClient()
  const q = useQuery({ queryKey: ['roomSettings'], queryFn: () => gqlClient.request(RoomDoc) })
  const current = q.data?.corrallm.usageRollup?.thermal
  const [form, setForm] = useState<Form>({
    ambientF: '',
    coolTargetF: '72',
    coolCostPerTherm: '',
    heatTargetF: '68',
    heatCostPerTherm: '',
    notes: '',
  })
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null)

  // Seed from what is stored, once it arrives.
  useEffect(() => {
    if (!current) return
    setForm({
      ambientF: String(current.ambientF),
      coolTargetF: String(current.coolTargetF),
      coolCostPerTherm: String(current.coolCostPerTherm),
      heatTargetF: String(current.heatTargetF),
      heatCostPerTherm: String(current.heatCostPerTherm),
      notes: current.notes ?? '',
    })
  }, [current])

  const done = (r: { ok?: boolean | null; message?: string | null } | null | undefined) => {
    setMsg({ ok: !!r?.ok, text: r?.message ?? '' })
    void qc.invalidateQueries({ queryKey: ['roomSettings'] })
    void qc.invalidateQueries({ queryKey: ['activity'] })
  }
  const save = useMutation({
    mutationFn: (clear: boolean) =>
      gqlClient.request(SetThermalDoc, {
        body: clear
          ? { clear: true, ambientF: 0, coolTargetF: 0, coolCostPerTherm: 0, heatTargetF: 0, heatCostPerTherm: 0 }
          : {
              ambientF: Number(form.ambientF),
              coolTargetF: Number(form.coolTargetF),
              coolCostPerTherm: Number(form.coolCostPerTherm),
              heatTargetF: Number(form.heatTargetF),
              heatCostPerTherm: Number(form.heatCostPerTherm),
              notes: form.notes,
            },
      }),
    onSuccess: (d) => done(d.corrallm.setThermal),
    onError: (e) => setMsg({ ok: false, text: String(e) }),
  })

  const field = (key: keyof Form, label: string, unit: string, help: string) => (
    <TextField
      size="small"
      label={`${label} (${unit})`}
      value={form[key]}
      onChange={(e) => setForm({ ...form, [key]: e.target.value })}
      helperText={help}
      type="number"
      sx={{ minWidth: 220, flex: '1 1 220px' }}
    />
  )
  const valid =
    form.ambientF !== '' &&
    Number(form.heatTargetF) > 0 &&
    Number(form.coolTargetF) > Number(form.heatTargetF) &&
    form.coolCostPerTherm !== '' &&
    form.heatCostPerTherm !== ''

  const modeWords: Record<string, string> = {
    cooling: 'Cooling — the box’s heat is extra load on the cooling.',
    heating: 'Heating — the box’s heat displaces some of the heating.',
    neutral: 'Between the targets — neither system runs; the heat costs and saves nothing.',
  }

  return (
    <Panel
      title="The room the box heats"
      subtitle={current ? modeWords[current.mode] ?? current.mode : 'not set — Traffic reports heat in BTU only'}
    >
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2, py: 1 }}>
        <Typography variant="body2" sx={{ color: C.textMuted }}>
          Every watt-hour local work draws ends up as heat in this room. Above the cooling target the
          room is being cooled, so that heat costs <b>heat ÷ 100,000 × cooling $/therm</b>; below the
          heating target it is being heated, so it saves <b>heat ÷ 100,000 × heating $/therm</b>;
          between them it is free. Price the source the thermostat actually turns down, not one that
          runs regardless.
        </Typography>
        <Stack direction="row" spacing={2} useFlexGap sx={{ flexWrap: 'wrap' }}>
          {field('ambientF', 'Ambient now', '°F', 'The room as you read it; update it as the season turns')}
        </Stack>
        <Stack direction="row" spacing={2} useFlexGap sx={{ flexWrap: 'wrap' }}>
          {field('coolTargetF', 'Cool above', '°F', 'Above this the room is being cooled')}
          {field(
            'coolCostPerTherm',
            'Cooling cost',
            '$ per therm removed',
            'AC/heat pump: $/kWh × 29.31 ÷ COP. Window fan: kWh per therm moved × $/kWh, where a fan moves 1.08 × CFM × (inside − outside °F) BTU/h',
          )}
        </Stack>
        <Stack direction="row" spacing={2} useFlexGap sx={{ flexWrap: 'wrap' }}>
          {field('heatTargetF', 'Heat below', '°F', 'Below this the room is being heated')}
          {field(
            'heatCostPerTherm',
            'Heating worth',
            '$ per therm added',
            'Electric resistance: $/kWh × 29.31. Gas: $/therm ÷ burner efficiency. Heat pump: $/kWh × 29.31 ÷ COP',
          )}
        </Stack>
        <TextField
          size="small"
          label="Notes (how the factors were worked out)"
          value={form.notes}
          onChange={(e) => setForm({ ...form, notes: e.target.value })}
          multiline
          minRows={2}
        />
        <Stack direction="row" spacing={1}>
          <Button variant="contained" size="small" disabled={!valid || save.isPending} onClick={() => save.mutate(false)}>
            Save
          </Button>
          {current && (
            <Button size="small" color="warning" disabled={save.isPending} onClick={() => save.mutate(true)}>
              Clear
            </Button>
          )}
        </Stack>
        {msg && (
          <Alert severity={msg.ok ? 'success' : 'error'} onClose={() => setMsg(null)}>
            {msg.text}
          </Alert>
        )}
      </Box>
    </Panel>
  )
}
