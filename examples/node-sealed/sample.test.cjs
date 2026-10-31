const { test } = require('node:test');
const assert = require('node:assert/strict');
const add = require('./add.cjs');

test('addition uses the sealed project module', () => {
  assert.equal(add(2, 3), 5);
});
