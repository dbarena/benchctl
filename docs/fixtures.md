# Fixtures

Fixtures run a benchmark suite over the **Cartesian product** of parameter values. benchctl runs each benchmark once per combination of fixture values rather than once per iteration, so one scenario file can vary the client count, the dataset size, or both.

## Execution order

```
for iteration in iterations:
  for fixture_combo in fixtures.cartesian_product():
    for benchmark in benchmarks:
      run(benchmark, fixture_combo)
```

## Fixture types

Each of the three fixture types supports a concise **shorthand** form and a verbose **long form**.

### Constant

Produces a single fixed value. Use it to tag results with a parameter value without varying it.

**Shorthand:**
```yaml
suite:
  fixtures:
    - scale: 10
```

**Long form:**
```yaml
suite:
  fixtures:
    - scale:
        type: constant
        params:
          value: 10
```

### Range

Generates an inclusive integer sequence from `min` to `max` with an optional `step` (default: 1).

**Shorthand** (use `from:`/`to:` as siblings of the fixture name):
```yaml
suite:
  fixtures:
    - clients:
      from: 1
      to: 8
```

**Long form:**
```yaml
suite:
  fixtures:
    - clients:
        type: range
        params:
          min: 1
          max: 8
          step: 2
```

The example above (step=2) produces: `1, 3, 5, 7`.

### List

Produces a fixed set of values in declaration order.

**Shorthand:**
```yaml
suite:
  fixtures:
    - protocol: [simple, extended]
```

**Long form:**
```yaml
suite:
  fixtures:
    - protocol:
        type: list
        params:
          values: [simple, extended]
```

---

## Using fixtures in benchmark steps

Reference fixture values inside step args with `{{ fixture.<name> }}`:

```yaml
suite:
  fixtures:
    - scale: 10
    - clients:
      from: 1
      to: 4

  benchmarks:
    - name: tpcc
      steps:
        - name: benchmark
          type: go-tpc
          command: run
          args:
            warehouses: "{{ fixture.scale }}"
            threads:    "{{ fixture.clients }}"
            time:       "{{ inputs.duration }}"
```

The four combinations (`clients` = 1, 2, 3, 4) each run the `tpcc` benchmark independently.

## Input references in fixture params

Fixture param values can reference scenario inputs using `{{ inputs.<name> }}`:

```yaml
inputs:
  max_clients:
    type: int
    default: 8

suite:
  fixtures:
    - clients:
      from: 1
      to: "{{ inputs.max_clients }}"
```

Override at runtime: `benchctl run scenario.yaml --set max_clients=4`

### Overriding a list fixture's values

A `list` fixture's `params.values` can also be a single `"{{ inputs.X }}"` reference to a
list-typed input, instead of a literal YAML sequence:

```yaml
inputs:
  threads:
    type: list
    default: [1, 2, 4, 8, 16, 24]

suite:
  fixtures:
    - threads:
        type: list
        params:
          values: "{{ inputs.threads }}"
```

A `list`-typed input round-trips as a comma-separated string. Write its default either as a
YAML sequence (as above) or as a literal string (`default: "1,2,4,8,16,24"`), and override it with
a comma-separated value, the same as any other input:

```bash
benchctl run scenario.yaml --set threads=1,2,4,8,16,24
```

---

## Labels and metrics

benchctl adds every fixture value as a **label** on all collected metrics, using the prefix `fixture_<name>`:

| Fixture | Label key |
|---------|-----------|
| `scale` | `fixture_scale` |
| `clients` | `fixture_clients` |
| `protocol` | `fixture_protocol` |

These labels appear in both the **VictoriaMetrics** push (as Prometheus label dimensions) and the **stdout** collector's result output.

### Stdout file names

With `file_enabled: true` on the stdout collector, each JSON result file carries the fixture values in its name, which keeps the names unique across combinations:

```
results_tpcc_1_1_simple.json   # iteration=1, clients=1, protocol=simple
results_tpcc_1_1_extended.json
results_tpcc_1_2_simple.json
...
```

## Demo: echo all combinations

The scenario `scenarios/echo-fixtures-demo.yaml` demonstrates fixtures locally without any infrastructure:

```yaml
apiVersion: bench/v1
kind: Scenario
metadata:
  name: echo-fixtures-demo

inputs:
  max_clients:
    type: int
    default: 4
  protocol:
    type: list
    default: [simple, extended]

target:
  provider: noop

suite:
  fixtures:
    - scale: 10
    - clients:
      from: 1
      to: "{{ inputs.max_clients }}"
    - protocol:
        type: list
        params:
          values: "{{ inputs.protocol }}"

  benchmarks:
    - name: echo
      steps:
        - name: print-combo
          type: shell
          args:
            command: "echo scale={{ fixture.scale }} clients={{ fixture.clients }} protocol={{ fixture.protocol }}"

driver:
  provider: local

collector:
  provider: stdout
```

**Run:**
```bash
# Default: 1 × 4 × 2 = 8 combinations
benchctl run scenarios/echo-fixtures-demo.yaml

# Override the range fixture's bound: 1 × 2 × 2 = 4 combinations
benchctl run scenarios/echo-fixtures-demo.yaml --set max_clients=2

# Override the list fixture's values: 1 × 4 × 3 = 12 combinations
benchctl run scenarios/echo-fixtures-demo.yaml --set protocol=simple,extended,binary
```

**Expected output** (8 lines):
```
scale=10 clients=1 protocol=simple
scale=10 clients=1 protocol=extended
scale=10 clients=2 protocol=simple
scale=10 clients=2 protocol=extended
scale=10 clients=3 protocol=simple
scale=10 clients=3 protocol=extended
scale=10 clients=4 protocol=simple
scale=10 clients=4 protocol=extended
```

## Previewing combinations without running

`benchctl info` resolves each fixture's declared values, including any `{{ inputs.X }}`
references, and reports the total number of combinations without running anything:

```bash
benchctl info scenarios/echo-fixtures-demo.yaml
```

```
Fixtures:
scale (constant)   10
clients (range)    1..4 (4 values)
protocol (list)    simple, extended
Total combinations: 8
```

`--set` previews the effect of an override on the fixture expansion before committing to a run:

```bash
benchctl info scenarios/echo-fixtures-demo.yaml --set max_clients=2
```

```
Fixtures:
scale (constant)   10
clients (range)    1..2 (2 values)
protocol (list)    simple, extended
Total combinations: 4
```
