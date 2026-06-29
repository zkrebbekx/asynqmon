import { useEffect, useMemo } from "react";
import { useLocation } from "react-router-dom";

export function usePolling(doFn: () => void, interval: number) {
  useEffect(() => {
    doFn();
    const id = setInterval(doFn, interval * 1000);
    return () => clearInterval(id);
  }, [interval, doFn]);
}

export function useQuery(): URLSearchParams {
  const { search } = useLocation();
  return useMemo(() => new URLSearchParams(search), [search]);
}
