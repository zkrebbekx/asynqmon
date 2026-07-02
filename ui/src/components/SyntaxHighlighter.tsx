import { memo } from "react";
import { Light as SHL } from "react-syntax-highlighter";
import { atomOneLight, atomOneDark } from "react-syntax-highlighter/dist/esm/styles/hljs";
import json from "react-syntax-highlighter/dist/esm/languages/hljs/json";
import yaml from "react-syntax-highlighter/dist/esm/languages/hljs/yaml";
import { useIsDark } from "../hooks";

SHL.registerLanguage("json", json);
SHL.registerLanguage("yaml", yaml);

interface Props {
  language?: string;
  children: string;
}

function SyntaxHighlighter({ language = "json", children }: Props) {
  const isDark = useIsDark();

  return (
    <SHL
      language={language}
      style={isDark ? atomOneDark : atomOneLight}
      customStyle={{ borderRadius: "0.5rem", fontSize: "13px", margin: 0 }}
    >
      {children}
    </SHL>
  );
}

// Memoized: task tables render up to 100 of these per poll tick, and
// re-highlighting unchanged payloads is by far the most expensive render work.
export default memo(SyntaxHighlighter);
