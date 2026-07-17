# Global TUI Navigation Design

## Goal

Ensure a still-running Cicerone TUI can always be exited from the keyboard, including when it displays an error, and provide consistent Vim-style `h/j/k/l` navigation alongside arrow keys.

## Global Exit

`q` and `esc` are unconditional exit keys. The model handles them before confirmation, result-overlay, running-action, detail-view, focus, or ordinary navigation logic and returns Bubble Tea's quit command.

This behavior applies whenever the Bubble Tea event loop is alive: normal feed display, loading and synchronization, displayed errors, action confirmation, action progress, action failure, and detail views. It does not attempt to recover input after a Go panic or process termination because a terminated process cannot receive keys.

Because `esc` exits immediately, it no longer dismisses an overlay, cancels a confirmation, or returns from details. Other existing affirmative and negative controls remain available where applicable, and Vim navigation provides detail/focus movement.

## Vim and Arrow Navigation

Vertical selection accepts `j` and down arrow for the next feed group, and `k` and up arrow for the previous group.

Horizontal navigation accepts `h` and left arrow for left/back, and `l` and right arrow for right/forward:

- In wide layouts, left/back focuses the feed pane and right/forward focuses the inspector pane.
- In narrow layouts, right/forward opens the selected item's detail view and left/back returns to the feed.

Existing `tab` and `enter` navigation may remain as additional aliases. Navigation keys do not initiate Homebrew mutations.

## Help and Testing

Visible help text documents `h/j/k/l`, arrows, and unconditional `q`/`esc` exit behavior.

Table-driven model tests verify that both exit keys return a quit command from normal, error, confirmation, action-running, action-result, and detail states. Navigation tests verify `h/l` and left/right equivalence in wide and narrow layouts, while existing `j/k` and up/down behavior remains green. Tests operate only on model state with fakes and never touch Homebrew or user caches.
