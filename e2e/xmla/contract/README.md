# Reading ADOMD.NET's contracts instead of guessing them

The client deserialises with `DataContractJsonSerializer`, so the JSON member
names it will match are declared on concrete types inside the shipped
assembly — `[DataMember(Name=…)]`. They are **readable**, not searchable.

That matters because guessing them is expensive and quietly misleading.
`docs/32-xmla-plan.md` records four screens spent on the routing reply's shape,
one of which produced a conclusion that survived a round before being
disproved; a single assembly read then retired the whole line of enquiry and
showed the payload was a bare array with two of five members never sent.

It has since paid twice. The same read settled `generateastoken`'s response as
`MWCToken { Token }` in one run, and — as a check on the method itself — the
`MWCASTokenRequest` it dumped matches the request body captured on the wire in
screen 8, field for field.

## Run it

```bash
python3 e2e/xmla/contract/run.py            # everything token-related
python3 e2e/xmla/contract/run.py Rowset     # any substring of a type name
```

Prints, for each matching `[DataContract]`, the member names the client
matches on — and for any method whose name matches, its return type, which is
what names the contract to look for.

## When to reach for this

Before screening a payload shape. A screen is the right tool when the client's
behaviour is the unknown; it is the wrong tool when the answer is written down
in an assembly you already have.
