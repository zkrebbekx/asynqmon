import React from "react";
import { ChevronDown } from "lucide-react";
import { cn } from "@/lib/utils";

interface SelectProps {
  value?: string;
  defaultValue?: string;
  onValueChange?: (value: string) => void;
  disabled?: boolean;
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
  children?: React.ReactNode;
}

// Pull className off SelectTrigger child so callers keep styling the control.
function findTriggerClassName(children: React.ReactNode): string | undefined {
  let cls: string | undefined;
  React.Children.forEach(children, (child) => {
    if (React.isValidElement(child) && child.type === SelectTrigger) {
      cls = (child.props as any).className;
    }
  });
  return cls;
}

// Collect options by traversing SelectContent > SelectItem (and SelectGroup > SelectItem).
function collectOptions(children: React.ReactNode): { value: string; label: React.ReactNode; disabled?: boolean }[] {
  const opts: { value: string; label: React.ReactNode; disabled?: boolean }[] = [];
  React.Children.forEach(children, (child) => {
    if (!React.isValidElement(child)) return;
    const type = child.type;
    if (type === SelectContent || type === SelectGroup) {
      opts.push(...collectOptions((child.props as any).children));
    } else if (type === SelectItem) {
      const p = child.props as any;
      opts.push({ value: p.value, label: p.children, disabled: p.disabled });
    }
  });
  return opts;
}

// Native <select> implementation — avoids @radix-ui/react-select setRef infinite-loop bug.
export function Select({ value, defaultValue, onValueChange, disabled, children }: SelectProps) {
  const opts = collectOptions(children);
  const triggerClassName = findTriggerClassName(children);
  return (
    <div className="relative inline-flex items-center">
      <select
        value={value ?? defaultValue ?? ""}
        onChange={(e) => onValueChange?.(e.target.value)}
        disabled={disabled}
        className={cn(
          "appearance-none h-9 w-full rounded-md border border-[hsl(var(--input))] bg-[hsl(var(--background))] text-[hsl(var(--foreground))] px-3 py-2 pr-8 text-sm shadow-sm focus:outline-none focus:ring-1 focus:ring-[hsl(var(--ring))] disabled:cursor-not-allowed disabled:opacity-50 cursor-pointer",
          triggerClassName
        )}
      >
        {opts.map((opt) => (
          <option key={opt.value} value={opt.value} disabled={opt.disabled}>
            {typeof opt.label === "string" ? opt.label : opt.value}
          </option>
        ))}
      </select>
      <ChevronDown className="pointer-events-none absolute right-2 h-4 w-4 opacity-50" />
    </div>
  );
}

// Sub-components return null — Select renders everything from the traversal above.
export function SelectTrigger(_props: { children?: React.ReactNode; className?: string }) {
  return null;
}
export function SelectValue(_props: { placeholder?: string }) {
  return null;
}
export function SelectContent(_props: { children?: React.ReactNode; position?: string; className?: string }) {
  return null;
}
export function SelectItem(_props: { children?: React.ReactNode; value: string; disabled?: boolean; className?: string }) {
  return null;
}
export function SelectGroup(_props: { children?: React.ReactNode }) {
  return null;
}
export function SelectLabel(_props: { children?: React.ReactNode; className?: string }) {
  return null;
}
export function SelectSeparator(_props: { className?: string }) {
  return null;
}
export function SelectScrollUpButton(_props: { className?: string }) {
  return null;
}
export function SelectScrollDownButton(_props: { className?: string }) {
  return null;
}
