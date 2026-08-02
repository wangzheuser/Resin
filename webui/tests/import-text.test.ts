import assert from "node:assert/strict";
import test from "node:test";
import { mergeUniqueSubscriptionLines } from "../src/features/subscriptions/import-text.ts";

test("merges repeated text imports while preserving first-seen order", () => {
  const result = mergeUniqueSubscriptionLines(
    "first\r\nsecond\nfirst",
    ["\uFEFFsecond\nthird\n", "third\r\nfourth"],
  );

  assert.deepEqual(result, {
    content: "first\nsecond\nthird\nfourth",
    added: 2,
    duplicates: 2,
  });
});

test("ignores blank lines and reports a repeated file as duplicates", () => {
  const first = mergeUniqueSubscriptionLines("", ["one\n\ntwo"]);
  const repeated = mergeUniqueSubscriptionLines(first.content, ["one\r\ntwo\r\n"]);

  assert.deepEqual(first, { content: "one\ntwo", added: 2, duplicates: 0 });
  assert.deepEqual(repeated, { content: "one\ntwo", added: 0, duplicates: 2 });
});

test("merges a large import without changing order", () => {
  const lines = Array.from({ length: 55_000 }, (_, index) => `http://host-${index}:8080`);
  const result = mergeUniqueSubscriptionLines("", [lines.join("\r\n")]);

  assert.equal(result.added, lines.length);
  assert.equal(result.duplicates, 0);
  assert.equal(result.content, lines.join("\n"));
});
