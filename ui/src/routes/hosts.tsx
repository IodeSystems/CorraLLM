import { createFileRoute, redirect } from '@tanstack/react-router'

/**
 * /hosts is /machines now (P30 phase C).
 *
 * The page's subject grew past its name: "Hosts" named the config rows;
 * "Machines" names what the page is actually about — the hardware, what it has,
 * and what is on it. Everything that page stacked into one scroll (capacity,
 * agents, the memory ledger, residency, the fleet's toolchain) now hangs off
 * /machines/<name>, one machine at a time.
 *
 * The address stays. Links live in notes and chat history, and /hosts is what
 * people type from memory.
 */
export const Route = createFileRoute('/hosts')({
  beforeLoad: () => {
    throw redirect({ to: '/machines' })
  },
})
