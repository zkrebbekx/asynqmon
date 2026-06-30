import { useDispatch } from "react-redux";
import { Queue } from "../api";
import { deleteQueueAsync } from "../actions/queuesActions";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "./ui/dialog";
import { Button } from "./ui/button";

interface Props {
  queue: Queue | null;
  onClose: () => void;
}

export default function DeleteQueueConfirmationDialog({ queue, onClose }: Props) {
  const dispatch = useDispatch();

  const handleDelete = () => {
    if (queue) {
      dispatch(deleteQueueAsync(queue.queue) as any);
    }
    onClose();
  };

  return (
    <Dialog open={queue !== null} onOpenChange={(open) => !open && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Delete Queue</DialogTitle>
          <DialogDescription>
            Are you sure you want to delete queue <strong>{queue?.queue}</strong>?
            This action cannot be undone.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>Cancel</Button>
          <Button variant="destructive" onClick={handleDelete}>Delete</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
