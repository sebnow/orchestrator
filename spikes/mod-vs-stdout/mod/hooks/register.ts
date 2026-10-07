import type { EngineInterface, Register } from 'claude-code'

// Spike probe: reports every hook event it can see to a local receiver
// (SPIKE_RECEIVER_URL) and takes commands from it. Not production code.

const NAME = 'spike-probe'
const MAX_STRING = 2000
const POLL_MS = 250

let seq = 0
let receiver = ''
let currentTurnId: string | undefined
let polling = false
// SPIKE_QUIET=1 stops reporting per-tool and per-command descriptions and the
// $ calls of other mods, which otherwise flood the receiver at start-up.
let quiet = false
const QUIET_SKIP = new Set(['tool.describe', 'command.describe', 'agent.offer'])
// Set by the abort-after-tool command: the next tool.call to return ends the turn.
let abortAfterTool = false

function clip(_key: string, value: unknown): unknown {
  if (typeof value === 'string' && value.length > MAX_STRING) {
    return value.slice(0, MAX_STRING) + `...[+${value.length - MAX_STRING} chars]`
  }
  return value
}

// Awaited by callers so the receiver sees events in the order the engine
// raised them; the measured latency includes this round trip.
async function post($: EngineInterface, event: string, phase: string, data: unknown, extra: Record<string, unknown> = {}) {
  if (!receiver) return
  const body = JSON.stringify({ seq: ++seq, sentAt: Date.now(), event, phase, ...extra, data }, clip)
  try {
    await $.http.fetch(receiver + '/event', { method: 'POST', headers: { 'content-type': 'application/json' }, body })
  } catch {
    // The receiver being gone must not break the session.
  }
}

async function ask($: EngineInterface, path: string, payload: unknown): Promise<any> {
  const r = await $.http.fetch(receiver + path, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(payload, clip),
  })
  return JSON.parse(r.text || '{}')
}

async function snapshot($: EngineInterface) {
  const [id, model, turns, usage, version] = await Promise.all([
    $.session.id(),
    $.session.model(),
    $.session.turns(),
    $.session.usage(),
    $.session.version(),
  ])
  return { id, model, turns, usage, version }
}

async function runCommand($: EngineInterface, cmd: any) {
  await post($, 'spike.command', 'received', cmd)
  if (cmd.type === 'abort') {
    try {
      if (!currentTurnId) throw new Error('no running turn recorded')
      await $.turn.abort({ turnId: currentTurnId })
      await post($, 'spike.command', 'done', { type: 'abort', turnId: currentTurnId })
    } catch (err) {
      await post($, 'spike.command', 'error', { type: 'abort', message: String(err) })
    }
  } else if (cmd.type === 'submit') {
    // Resolves when the submitted turn starts, so it is not awaited here.
    $.prompt.submit({ text: cmd.text, asUser: cmd.asUser === true }).then(
      (r) => post($, 'spike.command', 'submit-resolved', r),
      (err) => post($, 'spike.command', 'error', { type: 'submit', message: String(err) }),
    )
    await post($, 'spike.command', 'submit-called', { text: cmd.text })
  } else if (cmd.type === 'append') {
    try {
      const r = await $.session.append({ message: { type: 'user', content: [{ type: 'text', text: cmd.text }] } })
      await post($, 'spike.command', 'done', { type: 'append', result: r })
    } catch (err) {
      await post($, 'spike.command', 'error', { type: 'append', message: String(err) })
    }
  } else if (cmd.type === 'abort-after-tool') {
    abortAfterTool = true
    await post($, 'spike.command', 'done', { type: 'abort-after-tool', armed: true })
  } else if (cmd.type === 'snapshot') {
    await post($, 'spike.command', 'done', await snapshot($))
  }
}

async function abortAtToolBoundary($: EngineInterface, e: any) {
  abortAfterTool = false
  try {
    if (!currentTurnId) throw new Error('no running turn recorded')
    await post($, 'spike.command', 'abort-after-tool', { tool_use_id: e.tool_use_id, turnId: currentTurnId })
    await $.turn.abort({ turnId: currentTurnId })
    await post($, 'spike.command', 'done', { type: 'abort-after-tool', turnId: currentTurnId })
  } catch (err) {
    await post($, 'spike.command', 'error', { type: 'abort-after-tool', message: String(err) })
  }
}

async function poll($: EngineInterface) {
  if (polling || !receiver) return
  polling = true
  try {
    const r = await $.http.fetch(receiver + '/command')
    const cmds = JSON.parse(r.text || '[]')
    for (const cmd of cmds) await runCommand($, cmd)
  } catch (err) {
    await post($, 'spike.poll', 'error', { message: String(err) })
  } finally {
    polling = false
  }
}

export const register: Register = (on) => {
  // Every event except the streaming turn.step, which needs a generator.
  on('!turn.step', async ($, e: any, next: any) => {
    const event: string = next.event
    // Our own $ calls are events too; reporting them would only echo the probe.
    if (next.origin?.plugin === NAME || event === 'process.spawn') return next(e)
    // $ is empty while the engine builds it.
    if (event === 'engine.create') return next(e)

    if (!receiver) {
      receiver = (await $.env.get('SPIKE_RECEIVER_URL')) ?? ''
      quiet = (await $.env.get('SPIKE_QUIET')) === '1'
    }
    if (quiet && (QUIET_SKIP.has(event) || next.origin?.plugin !== 'engine')) return next(e)

    if (event === 'session.start') {
      await post($, event, 'before', e, { origin: next.origin })
      const r = await next(e)
      await post($, 'spike.snapshot', 'session.start', await snapshot($))
      $.clock.every(POLL_MS, () => void poll($))
      return r
    }

    await post($, event, 'before', e, { origin: next.origin })
    if (event === 'turn.start') currentTurnId = e.turnId

    const t0 = Date.now()
    let r = await next(e)
    await post($, event, 'after', r, { origin: next.origin, ms: Date.now() - t0 })

    if (event === 'tool.check') {
      // The receiver plays the daemon: it answers with a verdict or passes.
      try {
        const verdict = await ask($, '/decide', { tool: e.tool, input: e.input, tool_use_id: e.tool_use_id, core: r })
        if (verdict && verdict.decision) {
          r = { decision: verdict.decision, reason: verdict.reason }
          await post($, event, 'answered', r)
        }
      } catch (err) {
        await post($, event, 'decide-error', { message: String(err) })
      }
    }

    if (event === 'tool.call' && abortAfterTool) await abortAtToolBoundary($, e)

    if (event === 'turn.complete') {
      await post($, 'spike.snapshot', 'turn.complete', {
        ...(await snapshot($)),
        messages: (await $.session.messages()).slice(-4),
      })
      if (e.turnId === currentTurnId) currentTurnId = undefined
    }
    return r
  })

  on('turn.step', async function* ($, e, next) {
    await post($, 'turn.step', 'before', e)
    const it = next(e)[Symbol.asyncIterator]()
    let engineChunks = 0
    for (;;) {
      const step = await it.next()
      if (step.done) {
        await post($, 'turn.step', 'after', step.value, { engineChunks })
        return step.value
      }
      const c: any = step.value
      if (c.kind === 'engine') engineChunks++
      else await post($, 'turn.step', 'chunk', c)
      yield c
    }
  })
}
