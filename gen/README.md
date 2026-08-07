# Generated code

**Do not edit anything in here.** Every file is produced from `contracts/` by
`make contracts-generate`, and `make contracts-verify` fails CI if the committed
output differs from a fresh regeneration.

## Why this is committed rather than gitignored

- A reviewer sees the actual types in a pull request diff. "The contract
  changed" and "here is exactly what that does to the Go structs" arrive
  together instead of one being invisible.
- A contributor can build without installing three code generators.
- The drift check is what stops the committed copy from quietly becoming a lie,
  so we get the readability without the usual staleness cost.

## Layout

```
gen/go/events/         structs from contracts/events/*.schema.json
gen/go/taskingapi/     types + chi server interface from the OpenAPI document
gen/python/            Pydantic v2 models from the same event schemas
```
