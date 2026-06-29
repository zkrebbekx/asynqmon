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

  it("calls onValueChange with the selected value", async () => {
    const onChange = vi.fn();
    render(<Sample onChange={onChange} />);
    const select = screen.getByRole("combobox");
    await userEvent.selectOptions(select, "last-30d");
    expect(onChange).toHaveBeenCalledWith("last-30d");
  });
});
