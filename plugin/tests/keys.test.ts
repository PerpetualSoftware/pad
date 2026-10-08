import { expect, test } from 'claude-code/testing'
import { arrowStep, tabStep } from '../hooks/register.js'

// The Browse view as drawn: six tabs, two collection rows, then the footer actions.
const ORDER = ['tab-now', 'tab-next', 'tab-browse', 'tab-find', 'tab-activity', 'tab-settings', 'coll-tasks', 'coll-conventions', 'refresh', 'add']

test('Tab and Shift+Tab walk the menu with the list as one stop', async () => {
  // Tab from a row (the engine aims at the next row) leaves the list: nothing follows it, so it wraps to the first tab
  expect(tabStep(ORDER, 'coll-tasks', 'coll-conventions', 'coll-tasks')).toEqual({ element: 'tab-now', engineStop: false })
  // Tab across the menu
  expect(tabStep(ORDER, 'tab-now', 'tab-next', 'coll-tasks')).toEqual({ element: 'tab-next', engineStop: false })
  // Tab from the last tab enters the list on the row it left
  expect(tabStep(ORDER, 'tab-settings', 'coll-tasks', 'coll-conventions')).toEqual({ element: 'coll-conventions', engineStop: false })
  // Shift+Tab from a row (the engine aims at the row above, or the last tab) goes to the last tab
  expect(tabStep(ORDER, 'coll-conventions', 'coll-tasks', 'coll-conventions')).toEqual({ element: 'tab-settings', engineStop: false })
  // Shift+Tab from the first tab (the engine wraps to the last control) comes back into the list
  expect(tabStep(ORDER, 'tab-now', 'add', 'coll-tasks')).toEqual({ element: 'coll-tasks', engineStop: false })
  // ... or, when the engine aims at its own close mark, the same, flagged so the hook moves it itself
  expect(tabStep(ORDER, 'tab-now', undefined, 'coll-tasks')).toEqual({ element: 'coll-tasks', engineStop: true })
  // a click (not one step away) lands where it was aimed
  expect(tabStep(ORDER, 'tab-now', 'coll-conventions', 'coll-tasks')).toBeNull()
})

test('Up and Down walk the rows; page moves and the list edges still scroll', async () => {
  expect(arrowStep(ORDER, 'coll-tasks', 1, 'coll-tasks')).toBe('coll-conventions')
  expect(arrowStep(ORDER, 'coll-conventions', -1, 'coll-tasks')).toBe('coll-tasks')
  expect(arrowStep(ORDER, 'coll-conventions', 1, 'coll-tasks')).toBeNull() // bottom: scroll
  expect(arrowStep(ORDER, 'coll-tasks', -1, 'coll-tasks')).toBeNull() // top: scroll the header into view
  expect(arrowStep(ORDER, 'tab-browse', 1, 'coll-conventions')).toBe('coll-conventions') // Down from the menu enters the list
  expect(arrowStep(ORDER, 'coll-tasks', 30, 'coll-tasks')).toBeNull() // a page key
  expect(arrowStep(['tab-now', 'refresh'], 'tab-now', 1, undefined)).toBeNull() // a view with no list
})
