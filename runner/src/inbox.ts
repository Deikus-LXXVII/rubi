// Unbounded async queue used as the long-lived prompt of one SDK query
// (pattern from T3 Code, MIT: one query per thread fed by a queue that never ends by itself).
export class Inbox<T> implements AsyncIterable<T> {
  private readonly items: T[] = [];
  private wake: (() => void) | null = null;
  private ended = false;

  push(item: T): boolean {
    if (this.ended) return false;
    this.items.push(item);
    this.wake?.();
    return true;
  }

  end() {
    this.ended = true;
    this.wake?.();
  }

  [Symbol.asyncIterator](): AsyncIterator<T> {
    return {
      next: async () => {
        for (;;) {
          const v = this.items.shift();
          if (v !== undefined) return { value: v, done: false };
          if (this.ended) return { value: undefined, done: true };
          await new Promise<void>((r) => (this.wake = r));
          this.wake = null;
        }
      },
      return: async () => {
        this.end();
        return { value: undefined, done: true };
      },
    };
  }
}
