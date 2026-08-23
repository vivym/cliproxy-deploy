# Lark policy runtime directory

Place the reviewed `*.policy.json` bundles and `approval-bindings.json` in this
directory before enabling the Compose `lark` profile. The Controller validates
the complete historical catalog and fails closed when the active policy or an
approval binding is missing or inconsistent.

Policy bundles are deployment configuration and may be committed after review.
Do not place credentials or raw approval payloads here.
