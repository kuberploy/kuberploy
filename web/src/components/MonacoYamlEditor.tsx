import EditorWorker from "../monaco-worker?worker";
import { useEffect, useRef, useState } from "react";
import { cn } from "@/lib/utils";

type MonacoGlobal = typeof globalThis & {
  MonacoEnvironment?: { getWorker: () => Worker };
};
(globalThis as MonacoGlobal).MonacoEnvironment = {
  getWorker: () => new EditorWorker(),
};

export function MonacoYamlEditor({
  value,
  onChange,
  readOnly = false,
  ariaLabel = "App configuration YAML",
}: {
  value: string;
  onChange: (value: string) => void;
  readOnly?: boolean;
  ariaLabel?: string;
}) {
  const hostRef = useRef<HTMLDivElement>(null);
  const editorRef = useRef<
    import("monaco-editor").editor.IStandaloneCodeEditor | null
  >(null);
  const changeRef = useRef(onChange);
  const [ready, setReady] = useState(false);
  changeRef.current = onChange;

  useEffect(() => {
    let disposed = false;
    let subscription: { dispose: () => void } | undefined;
    void import("monaco-editor/editor/editor.api").then((monaco) => {
      if (disposed || !hostRef.current) return;
      const editor = monaco.editor.create(hostRef.current, {
        value,
        language: "yaml",
        theme:
          document.documentElement.dataset.theme === "dark" ? "vs-dark" : "vs",
        readOnly,
        minimap: { enabled: false },
        fontFamily:
          "'SFMono-Regular', 'Cascadia Code', 'Roboto Mono', monospace",
        fontSize: 13,
        lineHeight: 21,
        padding: { top: 16, bottom: 16 },
        scrollBeyondLastLine: false,
        automaticLayout: true,
        wordWrap: "on",
        accessibilitySupport: "on",
        ariaLabel,
        renderLineHighlight: "line",
      });
      editorRef.current = editor;
      subscription = editor.onDidChangeModelContent(() =>
        changeRef.current(editor.getValue()),
      );
      setReady(true);
    });
    return () => {
      disposed = true;
      subscription?.dispose();
      editorRef.current?.dispose();
      editorRef.current = null;
    };
  }, [ariaLabel, readOnly]);

  useEffect(() => {
    const applyEditorTheme = () => {
      const theme = document.documentElement.dataset.theme;
      void import("monaco-editor/editor/editor.api").then((monaco) =>
        monaco.editor.setTheme(theme === "dark" ? "vs-dark" : "vs"),
      );
    };
    applyEditorTheme();
    const observer = new MutationObserver(applyEditorTheme);
    observer.observe(document.documentElement, {
      attributes: true,
      attributeFilter: ["data-theme"],
    });
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    const editor = editorRef.current;
    if (editor && editor.getValue() !== value) editor.setValue(value);
  }, [value]);

  return (
    <div className="relative h-[520px]">
      {!ready ? (
        <textarea
          className="w-full h-full p-4 resize-none text-ink border-0 rounded-[0] bg-surface font-mono text-xs leading-[1.7]"
          aria-label={ariaLabel}
          value={value}
          readOnly={readOnly}
          onChange={(event) => onChange(event.target.value)}
        />
      ) : null}
      <div
        ref={hostRef}
        className={cn("absolute inset-0 opacity-0", ready && "opacity-100")}
      />
    </div>
  );
}
