// Global keyboard model (build contract §4.5): key normalization, the
// typing-context guard ("never hijack keys while typing in inputs/textareas/
// selects"), and a declarative dispatcher. Pure functions over DOM events so
// the guards are unit-testable without React.

/** True when the event target is a place the user types — plain keys must
 * never be hijacked there. */
export function isTypingContext(target: EventTarget | null): boolean {
  if (!target || !(target instanceof HTMLElement)) return false;
  const tag = target.tagName;
  if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return true;
  return target.isContentEditable === true;
}

/**
 * keyOf normalizes a KeyboardEvent to a chord string:
 *
 *   "mod+k"    ⌘K (mac) or Ctrl+K — both map to "mod"
 *   "shift+x"  shifted letters
 *   "j" "/" "?" "#" "[" "]"  printable keys as e.key reports them
 *   "escape"   named keys, lowercased
 *
 * Symbols produced WITH shift ("?", "#") keep their e.key form — no "shift+"
 * prefix, since the symbol itself already encodes it. Alt/Option chords are
 * out of vocabulary and return "" (never match).
 */
export function keyOf(e: KeyboardEvent): string {
  if (e.altKey) return "";
  const k = e.key;
  if (e.metaKey || e.ctrlKey) {
    if (k.length === 1) return `mod+${k.toLowerCase()}`;
    return ""; // mod+named-key chords are out of vocabulary
  }
  if (k.length === 1) {
    // Shifted letters normalize to "shift+<lower>"; shifted symbols are
    // already distinct characters.
    if (e.shiftKey && k >= "A" && k <= "Z") return `shift+${k.toLowerCase()}`;
    return k;
  }
  return k.toLowerCase();
}

export interface KeyBinding {
  onTrigger: (e: KeyboardEvent) => void;
  /** Fire even when focus is in an input/textarea/select (⌘K). */
  allowInInput?: boolean;
  /** Fire even while a palette/cheatsheet overlay is open (toggles). */
  allowInOverlay?: boolean;
}

export type Keymap = Record<string, KeyBinding | ((e: KeyboardEvent) => void)>;

/** The overlay marker GlobalShortcuts stamps on <body> while the palette or
 * cheat-sheet is open, so page-level bindings go quiet underneath. */
export const OVERLAY_ATTR = "data-fc-overlay-open";

function normalize(b: KeyBinding | ((e: KeyboardEvent) => void)): KeyBinding {
  return typeof b === "function" ? { onTrigger: b } : b;
}

/**
 * dispatchKey fires the binding for the event's chord, honoring the guards:
 *  - already-handled events (defaultPrevented) never re-fire;
 *  - typing contexts are skipped unless the binding opts in;
 *  - open overlays mute page bindings unless the binding opts in.
 * Returns true when a binding fired (callers preventDefault on true).
 */
export function dispatchKey(map: Keymap, e: KeyboardEvent): boolean {
  if (e.defaultPrevented) return false;
  const chord = keyOf(e);
  if (chord === "") return false;
  const raw = map[chord];
  if (!raw) return false;
  const binding = normalize(raw);
  if (!binding.allowInInput && isTypingContext(e.target)) return false;
  if (!binding.allowInOverlay && typeof document !== "undefined" && document.body.hasAttribute(OVERLAY_ATTR)) {
    return false;
  }
  binding.onTrigger(e);
  return true;
}
