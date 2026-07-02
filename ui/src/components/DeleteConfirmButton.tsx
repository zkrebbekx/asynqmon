import { useState } from "react";
import { Trash2 } from "lucide-react";
import { Button } from "./ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "./ui/tooltip";
import ConfirmDialog from "./ConfirmDialog";
import { cn } from "../lib/utils";

interface Props {
  description: React.ReactNode;
  onDelete: () => void;
  disabled?: boolean;
  className?: string;
}

// Per-row delete icon button that asks for confirmation before firing —
// deletes are irreversible and the rest of the row actions are one click.
export default function DeleteConfirmButton({ description, onDelete, disabled, className }: Props) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <Tooltip>
        <TooltipTrigger asChild>
          <Button
            size="icon"
            variant="ghost"
            className={cn("h-7 w-7 text-red-500", className)}
            disabled={disabled}
            onClick={() => setOpen(true)}
          >
            <Trash2 size={13} />
          </Button>
        </TooltipTrigger>
        <TooltipContent>Delete</TooltipContent>
      </Tooltip>
      <ConfirmDialog
        open={open}
        title="Delete Task"
        description={description}
        onConfirm={() => {
          setOpen(false);
          onDelete();
        }}
        onClose={() => setOpen(false)}
      />
    </>
  );
}
