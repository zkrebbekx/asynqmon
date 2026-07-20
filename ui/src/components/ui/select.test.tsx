import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "./select";

function Sample({ onChange }: { onChange?: (v: string) => void }) {
  return (
    <Select value="last-7d" onValueChange={onChange}>
      <SelectTrigger>
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value="today">Today</SelectItem>
        <SelectItem value="last-7d">Last 7d</SelectItem>
        <SelectItem value="last-30d">Last 30d</SelectItem>
      </SelectContent>
    </Select>
  );
}

describe("Select (native wrapper)", () => {
  it("renders an <option> for each SelectItem child", () => {
    render(<Sample />);
    const options = screen.getAllByRole("option") as HTMLOptionElement[];
    expect(options).toHaveLength(3);
    expect(options.map((o) => o.value)).toEqual(["today", "last-7d", "last-30d"]);
    expect(options.map((o) => o.textContent)).toEqual(["Today", "Last 7d", "Last 30d"]);
  });

  it("reflects the controlled value", () => {
    render(<Sample />);
    const select = screen.getByRole("combobox") as HTMLSelectElement;
    expect(select.value).toBe("last-7d");
  });

  it("flattens non-string SelectItem children into the option label", () => {
    render(
      <Select value="a">
        <SelectContent>
          <SelectItem value="a">
            <span>Wrapped Label</span>
          </SelectItem>
        </SelectContent>
      </Select>
    );
    const option = screen.getByRole("option") as HTMLOptionElement;
    expect(option.textContent).toBe("Wrapped Label");
  });

  // jsdom does no layout, so this asserts the cause rather than the clipping:
  // callers shrink the control (h-7/h-8) without touching padding, so a fixed
  // vertical padding leaves a content box shorter than the line box and the
  // label gets sliced off top and bottom.
  it("applies no vertical padding that a caller's height override would clip", () => {
    render(
      <Select value="last-7d">
        <SelectTrigger className="w-32 h-7 text-xs">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="last-7d">Last 7d</SelectItem>
        </SelectContent>
      </Select>
    );
    const select = screen.getByRole("combobox");
    expect(select.className).not.toMatch(/(^|\s)(py|pt|pb)-(?!0)/);
    expect(select.className).toContain("h-7");
  });

  it("calls onValueChange with the selected value", async () => {
    const onChange = vi.fn();
    render(<Sample onChange={onChange} />);
    const select = screen.getByRole("combobox");
    await userEvent.selectOptions(select, "last-30d");
    expect(onChange).toHaveBeenCalledWith("last-30d");
  });
});
