import { describe, it, expect, vi, afterEach } from "vitest";
import { dispatchKey, isTypingContext, keyOf, OVERLAY_ATTR, Keymap } from "./keymap";

function keyEvent(
  key: string,
  opts: Partial<KeyboardEventInit> = {},
  target?: HTMLElement
): KeyboardEvent {
  const e = new KeyboardEvent("keydown", { key, cancelable: true, ...opts });
  if (target) {
    // jsdom: target is read-only; dispatch through the element to set it.
    document.body.appendChild(target);
    target.dispatchEvent(e);
    target.remove();
  }
  return e;
}

afterEach(() => {
  document.body.removeAttribute(OVERLAY_ATTR);
});

describe("keyOf", () => {
  it("maps both ⌘K and Ctrl+K to mod+k", () => {
    expect(keyOf(keyEvent("k", { metaKey: true }))).toBe("mod+k");
    expect(keyOf(keyEvent("k", { ctrlKey: true }))).toBe("mod+k");
  });

  it("normalizes shifted letters to shift+<lower>", () => {
    expect(keyOf(keyEvent("X", { shiftKey: true }))).toBe("shift+x");
  });

  it("keeps shifted symbols as the symbol itself", () => {
    expect(keyOf(keyEvent("?", { shiftKey: true }))).toBe("?");
    expect(keyOf(keyEvent("#", { shiftKey: true }))).toBe("#");
  });

  it("passes plain printable keys through", () => {
    expect(keyOf(keyEvent("j"))).toBe("j");
    expect(keyOf(keyEvent("/"))).toBe("/");
    expect(keyOf(keyEvent("["))).toBe("[");
  });

  it("lowercases named keys and drops alt chords", () => {
    expect(keyOf(keyEvent("Escape"))).toBe("escape");
    expect(keyOf(keyEvent("j", { altKey: true }))).toBe("");
  });
});

describe("isTypingContext", () => {
  it.each(["input", "textarea", "select"])("is true inside a %s", (tag) => {
    expect(isTypingContext(document.createElement(tag))).toBe(true);
  });

  it("is true inside contentEditable and false elsewhere", () => {
    const div = document.createElement("div");
    expect(isTypingContext(div)).toBe(false);
    Object.defineProperty(div, "isContentEditable", { value: true });
    expect(isTypingContext(div)).toBe(true);
    expect(isTypingContext(null)).toBe(false);
    expect(isTypingContext(document.createElement("button"))).toBe(false);
  });
});

describe("dispatchKey", () => {
  it("fires the matching binding and reports it", () => {
    const fn = vi.fn();
    const map: Keymap = { j: fn };
    expect(dispatchKey(map, keyEvent("j"))).toBe(true);
    expect(fn).toHaveBeenCalledOnce();
  });

  it("never hijacks keys while typing in an input", () => {
    const fn = vi.fn();
    const map: Keymap = { "/": fn, j: fn };
    expect(dispatchKey(map, keyEvent("/", {}, document.createElement("input")))).toBe(false);
    expect(dispatchKey(map, keyEvent("j", {}, document.createElement("textarea")))).toBe(false);
    expect(fn).not.toHaveBeenCalled();
  });

  it("lets allowInInput bindings (⌘K) fire from an input", () => {
    const fn = vi.fn();
    const map: Keymap = { "mod+k": { onTrigger: fn, allowInInput: true } };
    const e = keyEvent("k", { metaKey: true }, document.createElement("input"));
    expect(dispatchKey(map, e)).toBe(true);
    expect(fn).toHaveBeenCalledOnce();
  });

  it("mutes page bindings while an overlay is open, unless opted in", () => {
    document.body.setAttribute(OVERLAY_ATTR, "1");
    const page = vi.fn();
    const toggle = vi.fn();
    const map: Keymap = {
      j: page,
      "mod+k": { onTrigger: toggle, allowInInput: true, allowInOverlay: true },
    };
    expect(dispatchKey(map, keyEvent("j"))).toBe(false);
    expect(dispatchKey(map, keyEvent("k", { ctrlKey: true }))).toBe(true);
    expect(page).not.toHaveBeenCalled();
    expect(toggle).toHaveBeenCalledOnce();
  });

  it("skips events something else already handled", () => {
    const fn = vi.fn();
    const e = keyEvent("j");
    e.preventDefault();
    expect(dispatchKey({ j: fn }, e)).toBe(false);
    expect(fn).not.toHaveBeenCalled();
  });

  it("returns false for chords outside the map", () => {
    expect(dispatchKey({}, keyEvent("q"))).toBe(false);
    expect(dispatchKey({ j: vi.fn() }, keyEvent("j", { altKey: true }))).toBe(false);
  });
});
