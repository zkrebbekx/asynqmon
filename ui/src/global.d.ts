declare global {
  interface Window {
    ROOT_PATH: string;
    PROMETHEUS_SERVER_ADDRESS: string;
    READ_ONLY: boolean;
    FLAG_ROOT_PATH?: string;
    FLAG_PROMETHEUS_SERVER_ADDRESS?: string;
    FLAG_READ_ONLY?: string;
  }
}

declare module "*.svg" {
  import React from "react";
  export const ReactComponent: React.FC<React.SVGProps<SVGSVGElement>>;
  const src: string;
  export default src;
}

export {};
