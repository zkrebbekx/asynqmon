// useKeymap binds a declarative keymap (lib/keymap) on document for the
// component's lifetime (build contract §4.5). The map lives in a ref so
// inline maps don't rebind the listener every render; guards (typing
// context, overlay mute) are enforced by dispatchKey.

import { useEffect, useRef } from "react";
import { Keymap, dispatchKey } from "../lib/keymap";

export function useKeymap(map: Keymap, enabled = true): void {
  const mapRef = useRef(map);
  mapRef.current = map;

  useEffect(() => {
    if (!enabled) return;
    const onKey = (e: KeyboardEvent) => {
      if (dispatchKey(mapRef.current, e)) e.preventDefault();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [enabled]);
}
