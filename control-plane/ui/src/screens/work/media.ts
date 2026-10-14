import { useEffect, useState } from "react";

// The boards' breakpoint: the desktop board at ≥900px, the phone board below.
// Used where the two boards draw different structures (Work's columns versus
// its filter chips), so only one of them is in the page at a time.
const query = "(min-width: 900px)";

function matches(): boolean {
  return typeof window === "undefined" || !window.matchMedia
    ? true
    : window.matchMedia(query).matches;
}

export function useDesktop(): boolean {
  const [desktop, setDesktop] = useState(matches);
  useEffect(() => {
    if (typeof window === "undefined" || !window.matchMedia) return;
    const list = window.matchMedia(query);
    const update = () => setDesktop(list.matches);
    update();
    list.addEventListener("change", update);
    return () => list.removeEventListener("change", update);
  }, []);
  return desktop;
}
