"""Fabric's notebook builtins, bound into this kernel.

On Fabric, `display` and `displayHTML` are provided BY THE RUNTIME — a notebook
writes `display(df)` with nothing above it — and `display` is Fabric's, not
IPython's. The two are not interchangeable: IPython's renders a DataFrame's
repr and answers `display(df, summary=True)` with a TypeError, so a notebook
authored against a stock kernel behaves differently on Fabric.

Shadowing IPython's name is therefore the fidelity choice, and it is kept
narrow: these two names, nothing else. Loaded as an IPython startup file so it
applies to every kernel this image starts, including the one nbclient spawns
for e2e/notebook-display.
"""
import notebook_display as _fabric_display

display = _fabric_display.display
displayHTML = _fabric_display.displayHTML
