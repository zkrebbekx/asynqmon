declare global {
  interface Window {
    ROOT_PATH: string;
    PROMETHEUS_SERVER_ADDRESS: string;
    READ_ONLY: boolean;
  }
}

export {};
