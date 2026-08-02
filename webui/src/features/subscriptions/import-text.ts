export type SubscriptionLineMergeResult = {
  content: string;
  added: number;
  duplicates: number;
};

function forEachNormalizedLine(content: string, visit: (line: string) => void): void {
  let start = content.charCodeAt(0) === 0xFEFF ? 1 : 0;
  for (let end = start; end <= content.length; end++) {
    if (end < content.length && content.charCodeAt(end) !== 10) {
      continue;
    }
    let lineEnd = end;
    if (lineEnd > start && content.charCodeAt(lineEnd - 1) === 13) {
      lineEnd--;
    }
    const line = content.slice(start, lineEnd).trim();
    if (line) {
      visit(line);
    }
    start = end + 1;
  }
}

export function mergeUniqueSubscriptionLines(
  current: string,
  imported: string[],
): SubscriptionLineMergeResult {
  const lines: string[] = [];
  const seen = new Set<string>();

  forEachNormalizedLine(current, (line) => {
    if (!seen.has(line)) {
      seen.add(line);
      lines.push(line);
    }
  });

  let added = 0;
  let duplicates = 0;
  for (const content of imported) {
    forEachNormalizedLine(content, (line) => {
      if (seen.has(line)) {
        duplicates++;
        return;
      }
      seen.add(line);
      lines.push(line);
      added++;
    });
  }

  return { content: lines.join("\n"), added, duplicates };
}
