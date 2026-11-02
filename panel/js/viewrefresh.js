// The refresh of a plugin page that reloads itself (its "refresh" seconds).
//
// There is at most one refresh waiting: arming one drops the one before. Every draw of a page arms its own, so
// without this a click (which draws the page again) or a refresh would add one more reload loop each time, and the
// page would soon redraw itself every moment, wiping what the user was doing.

export function createRefresher(setTimer = setTimeout, clearTimer = clearTimeout) {
  let timer = null;
  let gen = 0;
  return {
    // arm makes tick the only refresh to come, in seconds (at least 2).
    arm(seconds, tick) {
      if (timer !== null) clearTimer(timer);
      const mine = ++gen;
      timer = setTimer(() => {
        timer = null;
        if (mine === gen) tick();
      }, Math.max(Number(seconds) || 0, 2) * 1000);
    },
    // cancel drops the waiting refresh (another page is drawn).
    cancel() {
      if (timer !== null) clearTimer(timer);
      timer = null;
      gen++;
    },
  };
}

// unchanged says whether a refresh brought the same page as the one on screen: then nothing is redrawn.
export function unchanged(shownKey, shownJSON, key, json) {
  return shownKey === key && shownJSON === json;
}
