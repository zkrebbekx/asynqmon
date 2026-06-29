import { useState } from "react";
import { useSelector, useDispatch } from "react-redux";
import { AppState } from "../store";
import { pollIntervalChange, selectTheme } from "../actions/settingsActions";
import { ThemePreference } from "../reducers/settingsReducer";
import { Card, CardContent, CardHeader, CardTitle } from "../components/ui/card";
import { Label } from "../components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../components/ui/select";

export default function SettingsView() {
  const dispatch = useDispatch();
  const { pollInterval, themePreference } = useSelector((s: AppState) => s.settings);
  const [sliderValue, setSliderValue] = useState(pollInterval);

  return (
    <div className="max-w-2xl mx-auto px-4 py-8">
      <h1 className="text-2xl font-semibold mb-6 text-[hsl(var(--foreground))]">Settings</h1>

      <div className="space-y-4">
        <Card>
          <CardHeader>
            <CardTitle className="text-base font-medium text-[hsl(var(--foreground))]">Polling Interval</CardTitle>
          </CardHeader>
          <CardContent>
            <p className="text-sm text-[hsl(var(--muted-foreground))] mb-4">
              Web UI fetches live data every{" "}
              <strong>{sliderValue === 1 ? "1 second" : `${sliderValue} seconds`}</strong>
            </p>
            <input
              type="range"
              min={2}
              max={20}
              step={1}
              value={sliderValue}
              onChange={(e) => setSliderValue(Number(e.target.value))}
              onMouseUp={(e) => dispatch(pollIntervalChange(Number((e.target as HTMLInputElement).value)))}
              onTouchEnd={(e) => dispatch(pollIntervalChange(Number((e.target as HTMLInputElement).value)))}
              className="w-full accent-[hsl(var(--primary))]"
            />
            <div className="flex justify-between text-xs text-[hsl(var(--muted-foreground))] mt-1">
              <span>2s</span>
              <span>20s</span>
            </div>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-base font-medium text-[hsl(var(--foreground))]">Dark Theme</CardTitle>
          </CardHeader>
          <CardContent>
            <div className="flex items-center justify-between">
              <Label className="text-sm text-[hsl(var(--muted-foreground))]">Theme preference</Label>
              <Select
                value={String(themePreference)}
                onValueChange={(v) => dispatch(selectTheme(Number(v) as ThemePreference))}
              >
                <SelectTrigger className="w-48">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={String(ThemePreference.SystemDefault)}>System Default</SelectItem>
                  <SelectItem value={String(ThemePreference.Always)}>Always Dark</SelectItem>
                  <SelectItem value={String(ThemePreference.Never)}>Always Light</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
