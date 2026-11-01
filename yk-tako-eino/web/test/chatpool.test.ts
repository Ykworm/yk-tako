import { test } from 'node:test'
import assert from 'node:assert/strict'
import { pickEviction, type PoolEntry } from '../src/chatpool.ts'

const row = (sessionId: string | null, lastActiveAt: number, extra: Partial<PoolEntry> = {}): PoolEntry => ({
  sessionId,
  busy: false,
  dirty: false,
  lastActiveAt,
  ...extra,
})

test('没超上限不淘汰', () => {
  assert.equal(pickEviction([row('a', 1), row('b', 2)], 2, 0), -1)
})

test('取最久没激活的那个', () => {
  const entries = [row('a', 30), row('b', 10), row('c', 20)]
  assert.equal(pickEviction(entries, 2, 0), 1)
})

test('busy 与带草稿的不淘汰', () => {
  const entries = [row('a', 30), row('b', 10, { busy: true }), row('c', 20, { dirty: true })]
  assert.equal(pickEviction(entries, 2, 0), -1)
})

test('空白实例不在淘汰候选里（没 sid 不能重建）', () => {
  const entries = [row('a', 30), row(null, 5)]
  assert.equal(pickEviction(entries, 1, 0), -1)
})

test('激活实例自己不动', () => {
  const entries = [row('a', 1), row('b', 9)]
  assert.equal(pickEviction(entries, 1, 0), 1)
})
