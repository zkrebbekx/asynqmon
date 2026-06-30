import { Light as SHL } from "react-syntax-highlighter";
import { atomOneLight, atomOneDark } from "react-syntax-highlighter/dist/esm/styles/hljs";
import json from "react-syntax-highlighter/dist/esm/languages/hljs/json";
import yaml from "react-syntax-highlighter/dist/esm/languages/hljs/yaml";
import { useSelector } from "react-redux";
import { AppState } from "../store";
import { ThemePreference } from "../reducers/settingsReducer";

SHL.registerLanguage("json", json);
SHL.registerLanguage("yaml", yaml);

interface Props {
  language?: string;
  children: string;
}

export default function SyntaxHighlighter({ language = "json", children }: Props) {
  const themePreference = useSelector((s: AppState) => s.settings.themePreference);
  const prefersDark = window.matchMedia("(prefers-color-scheme: dark)").matches;
  const isDark =
    themePreference === ThemePreference.Always ||
    (themePreference === ThemePreference.SystemDefault && prefersDark);

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
