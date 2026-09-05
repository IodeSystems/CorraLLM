import { createFileRoute, redirect } from '@tanstack/react-router'

/**
 * /hosts is /machines now (P30 phase C).
 *
 * The page's subject grew past its name: it holds the declared capacity, the
 * attached agents, the toolchain each host builds with, the live memory ledger
 * (phase A) and what is resident on each box (phase C). "Hosts" named the config
 * rows; "Machines" names what the page is actually about — the hardware, what it
 * has, and what is on it.
 *
 * The address stays. Links live in notes and chat history.
 */
export const Route = createFileRoute('/hosts')({
  beforeLoad: () => {
    throw redirect({ to: '/machines' })
  },
})
