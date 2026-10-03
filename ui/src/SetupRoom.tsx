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
          roomF
          outdoorF
          coolTargetF
          coolCostPerTherm
          coolFanCfm
          coolFanWatts
          coolFactorNow
          coolBlocked
          latitude
          longitude
          outdoorSource
          outdoorNowF
          homeAssistantUrl
          roomEntity
          outdoorEntity
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
  roomF: string
  outdoorF: string
  latitude: string
  longitude: string
  homeAssistantUrl: string
  homeAssistantToken: string
  roomEntity: string
  outdoorEntity: string
  coolTargetF: string
  coolCostPerTherm: string
  coolFanCfm: string
  coolFanWatts: string
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
    roomF: '',
    outdoorF: '',
    latitude: '',
    longitude: '',
    homeAssistantUrl: '',
    homeAssistantToken: '${HA_TOKEN}',
    roomEntity: '',
    outdoorEntity: '',
    coolTargetF: '72',
    coolCostPerTherm: '',
    coolFanCfm: '',
    coolFanWatts: '',
    heatTargetF: '68',
    heatCostPerTherm: '',
    notes: '',
  })
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null)

  // Seed from what is stored, once it arrives.
  useEffect(() => {
    if (!current) return
    setForm({
      roomF: String(current.roomF),
      outdoorF: current.outdoorF ? String(current.outdoorF) : '',
      latitude: current.latitude ? String(current.latitude) : '',
      longitude: current.longitude ? String(current.longitude) : '',
      homeAssistantUrl: current.homeAssistantUrl ?? '',
      homeAssistantToken: '${HA_TOKEN}',
      roomEntity: current.roomEntity ?? '',
      outdoorEntity: current.outdoorEntity ?? '',
      coolTargetF: String(current.coolTargetF),
      coolCostPerTherm: current.coolCostPerTherm ? String(current.coolCostPerTherm) : '',
      coolFanCfm: current.coolFanCfm ? String(current.coolFanCfm) : '',
      coolFanWatts: current.coolFanWatts ? String(current.coolFanWatts) : '',
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
          ? { clear: true, roomF: 0, coolTargetF: 0, heatTargetF: 0, heatCostPerTherm: 0 }
          : {
              roomF: Number(form.roomF),
              outdoorF: Number(form.outdoorF || 0),
              latitude: Number(form.latitude || 0),
              longitude: Number(form.longitude || 0),
              ...(form.homeAssistantUrl
                ? {
                    homeAssistantUrl: form.homeAssistantUrl,
                    homeAssistantToken: form.homeAssistantToken,
                    roomEntity: form.roomEntity,
                    outdoorEntity: form.outdoorEntity,
                  }
                : {}),
              coolTargetF: Number(form.coolTargetF),
              coolCostPerTherm: Number(form.coolCostPerTherm || 0),
              coolFanCfm: Number(form.coolFanCfm || 0),
              coolFanWatts: Number(form.coolFanWatts || 0),
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
  const fan = Number(form.coolFanCfm || 0) > 0
  const located = Number(form.latitude || 0) !== 0 || Number(form.longitude || 0) !== 0
  const valid =
    form.roomF !== '' &&
    Number(form.heatTargetF) > 0 &&
    Number(form.coolTargetF) > Number(form.heatTargetF) &&
    (fan ? Number(form.coolFanWatts) > 0 && (located || form.outdoorF !== '') : form.coolCostPerTherm !== '') &&
    form.heatCostPerTherm !== ''

  const modeWords: Record<string, string> = {
    cooling: 'Cooling — the room is above its cooling target, so the box’s heat is extra load.',
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
          {field('roomF', 'Room now', '°F', 'Indoors, where the box is. Decides whether the room is being cooled or heated')}
          {field(
            'outdoorF',
            located ? 'Outdoor fallback' : 'Outdoor now',
            '°F',
            located
              ? 'Used only for an hour the weather service has no reading for'
              : 'Outside. Sets what a window fan can do: it cools only while outside is cooler than the room',
          )}
        </Stack>
        <Stack direction="row" spacing={2} useFlexGap sx={{ flexWrap: 'wrap' }}>
          {field('latitude', 'Latitude', '°', 'With longitude: hourly outdoor temperatures from Open-Meteo (free, no key), so cooling is priced hour by hour. Seattle ≈ 47.61')}
          {field('longitude', 'Longitude', '°', 'Seattle ≈ −122.33. Leave both empty to use the single outdoor reading')}
        </Stack>
        <Typography variant="overline" sx={{ color: C.textMuted }}>
          Home Assistant (hourly room, optional outdoor)
        </Typography>
        <Stack direction="row" spacing={2} useFlexGap sx={{ flexWrap: 'wrap' }}>
          <TextField
            size="small"
            label="Home Assistant URL"
            value={form.homeAssistantUrl}
            onChange={(e) => setForm({ ...form, homeAssistantUrl: e.target.value })}
            helperText="e.g. https://ha.example.com — empty to use the manual room temperature"
            sx={{ minWidth: 260, flex: '1 1 260px' }}
          />
          <TextField
            size="small"
            label="Token (a reference, not the token)"
            value={form.homeAssistantToken}
            onChange={(e) => setForm({ ...form, homeAssistantToken: e.target.value })}
            helperText="${KEY} naming a line in ~/.corrallm/credentials. The config is served over HTTP — never paste the token itself"
            sx={{ minWidth: 260, flex: '1 1 260px' }}
          />
        </Stack>
        <Stack direction="row" spacing={2} useFlexGap sx={{ flexWrap: 'wrap' }}>
          <TextField
            size="small"
            label="Room sensor entity"
            value={form.roomEntity}
            onChange={(e) => setForm({ ...form, roomEntity: e.target.value })}
            helperText="The room the box is in — or a whole-home average if air circulates between rooms"
            sx={{ minWidth: 260, flex: '1 1 260px' }}
          />
          <TextField
            size="small"
            label="Outdoor sensor entity (optional)"
            value={form.outdoorEntity}
            onChange={(e) => setForm({ ...form, outdoorEntity: e.target.value })}
            helperText="Used instead of the weather service when set"
            sx={{ minWidth: 260, flex: '1 1 260px' }}
          />
        </Stack>
        {current?.outdoorSource === 'open-meteo' || (current && (current.latitude !== 0 || current.longitude !== 0)) ? (
          <Typography variant="body2" sx={{ color: C.textMuted }}>
            {current.outdoorNowF
              ? `Outdoor now (Open-Meteo, last hour): ${current.outdoorNowF}°F.`
              : 'Weather is being fetched — the first hourly readings arrive within a few minutes of saving a location.'}
          </Typography>
        ) : null}
        <Stack direction="row" spacing={2} useFlexGap sx={{ flexWrap: 'wrap' }}>
          {field('coolTargetF', 'Cool above', '°F', 'Above this the room is being cooled')}
          {field('coolFanCfm', 'Cooling fan airflow', 'CFM', 'If cooling is a fan moving outside air: a 20" box fan is ~2,000 CFM. Leave empty for a fixed $/therm')}
          {field('coolFanWatts', 'Cooling fan draw', 'W', 'The fan\'s watts (~75 W for a box fan)')}
          {!fan &&
            field(
              'coolCostPerTherm',
              'Cooling cost',
              '$ per therm removed',
              'Without a fan: AC or heat pump at $/kWh × 29.31 ÷ COP',
            )}
        </Stack>
        {current && current.coolFanCfm > 0 && (
          <Typography variant="body2" sx={{ color: C.textMuted }}>
            {current.coolBlocked
              ? `Right now the fan cannot cool: outside (${current.outdoorF}°F) is not cooler than the room (${current.roomF}°F).`
              : `Right now the fan removes heat at $${current.coolFactorNow.toFixed(3)} per therm — a ${(current.roomF - current.outdoorF).toFixed(0)}°F gap moves ${Math.round(1.08 * current.coolFanCfm * (current.roomF - current.outdoorF)).toLocaleString()} BTU/h for ${current.coolFanWatts} W.`}
          </Typography>
        )}
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
