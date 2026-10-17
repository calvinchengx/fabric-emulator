"""Quote one SQL identifier, so a name cannot become statement.

WHY THIS EXISTS, and why it is a helper rather than a backtick typed at each
site. Every statement this agent builds names a table, a schema or a view by
splicing a string into SQL text, and a name spliced into SQL text is SQL
SYNTAX, not a value. Backticks around it look like quoting and are not: a
backtick INSIDE the name closes the pair, and whatever follows is parsed.

    name = "x` AS SELECT * FROM secrets --"
    f"CREATE OR REPLACE TEMP VIEW `{name}` AS {policy}"
    # -> CREATE OR REPLACE TEMP VIEW `x` AS SELECT * FROM secrets --` AS ...

THE ESCAPE IS DOUBLING, exactly as the Go side of this repository already does
it. `internal/tds.principalName` doubles `]` -> `]]` for a bracket-quoted T-SQL
identifier, and the matching `'` -> `''` for an N'...' literal is inline there
-- a local called `quotedID` in `EnsurePrincipal`, open-coded again in
`SyncOneLakeMemberships` and `SyncShortcutAccess`. `internal/server` has both
as named helpers, `sqlIdent`/`sqlLiteral`, and `enterDelegated` leans on SQL
Server's own `QUOTENAME`, which doubles `]` too. Correct at every site, and
re-established by reading rather than by a call -- which is the argument for
this side being one helper that every site goes through. Spark's quote character is the backtick, so the rule is `` ` `` -> ``` `` ```.
This module is that repair in Python, and the asymmetry is the reason it was
needed: when `sql-identifier-interpolated` (R8) was written, the Go half of
this tree escaped correctly at every site and NOT ONE backtick was doubled
anywhere in `python/spark_agent` -- including on the path that installs OneLake
row- and column-level security, where a name that breaks out of its statement
is an authorization failure rather than a syntax error.

WHERE THE NAMES COME FROM, which is what makes this reachable rather than
theoretical. A lakehouse table is a FOLDER under `Tables/` in OneLake, created
by whoever can write there; nothing in `internal/onelake` rejects a backtick in
a path segment. `catalog.register` turns that folder name into a catalog entry,
`SHOW TABLES` reads it back, and `onelake_security.apply` splices it into the
view that enforces the policy. The whole way round, the name is data chosen
outside this process.

IT FAILS CLOSED on a name carrying NUL or a line break. Neither can appear in a
real identifier, both can truncate or split a statement depending on who parses
it, and `apply`'s caller already treats an exception as "withhold the table" --
so raising is the direction that serves no rows. An EMPTY name is passed
through as an empty quoted identifier instead: it is a bug upstream rather than
an escape, and the engine refuses it with a message naming the statement, which
is more useful than a ValueError from here.
"""

# Doubling the quote character is the whole escape; it is a module-level
# constant so the test table and the callers cannot disagree about which
# character Spark quotes with.
QUOTE = "`"
_REFUSED = ("\x00", "\n", "\r")


def quote_ident(name):
    """`name` as one Spark identifier, safe to splice into SQL text.

    An ordinary name comes back byte-identical to the hand-written
    `` f"`{name}`" `` it replaces, which is what lets this be applied at every
    site without moving a single fixture or e2e output.
    """
    s = "" if name is None else str(name)
    for bad in _REFUSED:
        if bad in s:
            raise ValueError(
                "a SQL identifier cannot contain a NUL or a line break, and "
                "this one does, so no statement is built from it: "
                f"{s!r}. Refusing rather than emitting a statement whose "
                "parse depends on who is reading it.")
    return QUOTE + s.replace(QUOTE, QUOTE * 2) + QUOTE


def quote_literal(value):
    """`value` as one single-quoted SQL string literal.

    THE SECOND HALF OF THE MIRROR, and the reason it is here rather than left
    for later: the Go escape this module copies is a PAIR. `internal/server`
    has `sqlIdent` AND `sqlLiteral`; `internal/tds` has `principalName` AND
    `quotedID`. An identifier is quoted with a backtick and a literal with an
    apostrophe, and a repair that brought over only the identifier half would
    leave `LOCATION '{loc}'` -- on the same statements, built from the same
    catalog data -- able to close its quote and append a clause, which is the
    identical defect one character over.

    `''` is the doubling, as it is in `quotedID`. NUL and line breaks are
    refused for the same fail-closed reason as `quote_ident`: a path cannot
    legitimately carry either, and a statement whose parse depends on the
    reader is not one worth emitting.
    """
    s = "" if value is None else str(value)
    for bad in _REFUSED:
        if bad in s:
            raise ValueError(
                "a SQL string literal cannot contain a NUL or a line break, "
                f"and this one does, so no statement is built from it: {s!r}.")
    return "'" + s.replace("'", "''") + "'"
