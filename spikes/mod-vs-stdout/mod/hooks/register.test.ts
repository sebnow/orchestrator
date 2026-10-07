import { expect, test } from 'claude-code/testing'

test('tool.check takes the verdict the receiver answers', async ($, on) => {
  const posted: string[] = []
  on('env.get', async () => ({ value: 'http://receiver.test' }))
  on('http.fetch', async (_$, e: any) => {
    posted.push(e.url)
    if (e.url.endsWith('/decide')) {
      return { value: { status: 200, ok: true, headers: {}, text: '{"decision":"deny","reason":"daemon says no"}' } }
    }
    return { value: { status: 200, ok: true, headers: {}, text: '[]' } }
  })
  on('tool.check', async () => ({ decision: 'ask' }))

  const verdict = await $.tool.check({ tool: 'Bash', input: { command: 'touch x' } })

  expect(verdict.decision).toBe('deny')
  expect(verdict.reason).toBe('daemon says no')
  expect(posted).toContain('http://receiver.test/decide')
})
