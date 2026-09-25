def toCamelCase(s :str) -> str:
    result = ""
    capitalize_next = True
    for char in s:
        if char == '_' or char == '-' or char == ' ':
            capitalize_next = True
            continue
        if capitalize_next:
            result += char.upper()
            capitalize_next = False
        else:
            result += char
    return result

def append_metric(metric :dict,src_var :str | None = None) -> str:
    longName = metric.get("long_name", metric["name"])
    holdType = metric.get("hold_type", metric.get("type", "any"))
    typ = metric.get("type", "any")
    result = ""
    src_var = "s." + toCamelCase(longName) if src_var is None else src_var
    dst_var = "obj[Stat" + toCamelCase(longName) + "]"
    if holdType != typ:
        result += "    // convert " + holdType + " to " + typ + "\n"
        convertLogic = metric.get("to_objproto", "")
        if convertLogic:
            convertLogic = convertLogic.replace("$dst", dst_var).replace("$src", src_var)
            result += "    " + convertLogic + "\n"
        elif holdType.startswith("[]") and typ.startswith("[]"):
            inner_typ = typ[2:]
            result += "    {\n"
            result += "        converted := make([]" + inner_typ + ", len(" + src_var + "))\n"
            result += "        for i, v := range " + src_var + " {\n"
            result += "            converted[i] = " + inner_typ + "(v)\n"
            result += "        }\n"
            result += "        " + dst_var + " = converted\n"
            result += "    }\n"
        else:
            result += "    " + dst_var + " = " + typ + "(" + src_var + ")\n"
    else:
        result += "    " + dst_var + " = " + src_var + "\n"
    return result

def parse_metric(metric :dict,dst_var :str | None = None) -> str:
    longName = metric.get("long_name", metric["name"])
    holdType = metric.get("hold_type", metric.get("type", "any"))
    typ = metric.get("type", "any")
    result = ""
    dst_var = "s." + toCamelCase(longName) if dst_var is None else dst_var
    if holdType != typ:
        result += "        // convert " + typ + " to " + holdType + "\n"
        convertLogic = metric.get("from_objproto", "")
        if convertLogic:
            src_var = "v"
            convertLogic = convertLogic.replace("$dst", dst_var).replace("$src", src_var)
            result += "        " + convertLogic + "\n"
        elif holdType.startswith("[]") and typ.startswith("[]"):
            inner_typ = holdType[2:]
            result += "        {\n"
            result += "            parsedSlice := v\n"
            result += "            converted := make([]" + inner_typ + ", len(parsedSlice))\n"
            result += "            for i, val := range parsedSlice {\n"
            result += "                converted[i] = " + inner_typ + "(val)\n"
            result += "            }\n"
            result += "            " + dst_var + " = converted\n"
            result += "        }\n"
        else:
            result += "        " + dst_var + " = " + holdType + "(v)\n"
    else:
        result += "        " + dst_var + " = v\n"
    return result

def append_metric_string(metric :dict,src_var :str | None = None) -> str:
    display_name = metric.get("display_name", metric.get("long_name", metric["name"]))
    longName = metric.get("long_name", metric["name"])
    result = "    buf.WriteString(indent)\n"
    result += "    buf.WriteString(\"" + display_name + ": \")\n"
    default_fmt = "fmt.Sprintf(\"%v\", $src)"
    if "float" in metric.get("type", ""):
        default_fmt = "fmt.Sprintf(\"%.2f\", $src)"
    string_format = metric.get("string_format", default_fmt)
    string_format = string_format.replace("$src", "s." + toCamelCase(longName) if src_var is None else src_var)
    result += "    buf.WriteString(" + string_format + ")\n"
    result += "    buf.WriteString(\"\\n\")\n"
    return result

def process_stat_metrics(statMetrics :dict) -> tuple[str,dict]:
    result = ""
    # handle imports
    result += "import (\n"
    for imp in statMetrics.get("imports", []):
        result += "    \"" + imp + "\"\n"
    result += ")\n\n"
    for metric in statMetrics.get("metrics", []):
        longName = metric.get("long_name", metric["name"])
        result += "const Stat" + toCamelCase(longName) + " = \"" + str(metric["name"]) + "\"\n\n"
    # merge per category
    categories = {}
    for metric in statMetrics.get("metrics", []):
        category = metric["category"]
        if category not in categories:
            categories[category] = []
        categories[category].append(metric)
    for category, metrics in categories.items():
        result += "var StatMetrics" + toCamelCase(category) + " = []string{\n"
        for metric in metrics:
            longName = metric.get("long_name", metric["name"])
            result += "    Stat" + toCamelCase(longName) + ",\n"
        result += "}\n\n"
    # append string for debugging
    for metric in statMetrics.get("metrics", []):
        longName = metric.get("long_name", metric["name"])
        result += "func Append" + toCamelCase(longName) + "String(buf *strings.Builder, indent string, value " + metric.get("hold_type", metric.get("type", "any")) + ") {\n"
        result += append_metric_string(metric, "value")
        result += "}\n\n"

    # map per category struct
    for category, metrics in categories.items():
        result += "type " + toCamelCase(category) + "Stat struct {\n"
        for metric in metrics:
            longName = metric.get("long_name", metric["name"])
            holdType = metric.get("hold_type", metric.get("type", "any"))
            result += "    " + toCamelCase(longName) + " " + holdType + " `json:\"" + str(metric["name"]) + "\"`\n"
        result += "}\n\n"
    for category, metrics in categories.items():
        # Equal — field-by-field comparison using each metric's equal_check
        # (or slices.Equal for slices, == for scalars). Returns true iff
        # every field matches. Phase β replacement for AppendDiff len==0 check.
        result += "func (s *" + toCamelCase(category) + "Stat) Equal(other *" + toCamelCase(category) + "Stat) bool {\n"
        result += "    if other == nil {\n"
        result += "        return false\n"
        result += "    }\n"
        for metric in metrics:
            holdType = metric.get("hold_type", metric.get("type", "any"))
            longName = metric.get("long_name", metric["name"])
            equalCheckLogic = metric.get("equal_check", "").strip()
            if equalCheckLogic:
                src_var1 = "s." + toCamelCase(longName)
                src_var2 = "other." + toCamelCase(longName)
                logic = equalCheckLogic.replace("$src1", src_var1).replace("$src2", src_var2)
                result += "    if !(" + logic + ") {\n"
                result += "        return false\n"
                result += "    }\n"
            else:
                if holdType.startswith("[]"):
                    result += "    if !slices.Equal(s." + toCamelCase(longName) + ", other." + toCamelCase(longName) + ") {\n"
                    result += "        return false\n"
                    result += "    }\n"
                else:
                    result += "    if s." + toCamelCase(longName) + " != other." + toCamelCase(longName) + " {\n"
                    result += "        return false\n"
                    result += "    }\n"
        result += "    return true\n"
        result += "}\n\n"
        # AppendString for debugging
        result += "func (s *" + toCamelCase(category) + "Stat) AppendString(buf *strings.Builder,indent string) {\n"
        for metric in metrics:
            appendMetricStringFnName = "Append" + toCamelCase(metric.get("long_name", metric["name"])) + "String"
            result += "    " + appendMetricStringFnName + "(buf, indent, s." + toCamelCase(metric.get("long_name", metric["name"])) + ")\n"
        result += "}\n\n"
    return result,categories

def gen_sample_value(metric :dict,phase :int) -> str:
    sample_value = metric.get("sample_value", None)
    if sample_value is None:
        base_num = 10000000
        # if array/slice, set a default sample
        holdType = metric.get("hold_type", metric.get("type", "any"))
        if holdType.startswith("[]"):
            inner_typ = holdType[2:]
            if inner_typ == "string":
                sample_value = "[]" + inner_typ + "{\"sample" + str(phase) + "\"}\n"
            elif "float" in inner_typ:
                sample_value = "[]" + inner_typ + "{" + inner_typ + "(math.Pi + " + str(phase) + ")}\n"
            else:
                sample_value = "[]" + inner_typ + "{" + inner_typ + "("+str(base_num + phase)+")}\n"
        else:
            if holdType == "string":
                sample_value = "\"" + "sample" + str(phase) + "\"\n"
            elif holdType == "time.Time":
                sample_value = "time.Now().Add(time.Duration(" + str(phase) + ") * time.Minute)\n"
            elif "float" in holdType:
                sample_value = holdType + "(math.Pi + " + str(phase) + ")\n"
            else:
                sample_value = holdType + "(" + str(base_num + phase) + ")\n"
    else:
        assert isinstance(sample_value, list)
        sample_value = sample_value[phase]
    return sample_value

def objproto_compliance_test(c :dict,categories :dict) -> str:
    # Phase β2: drop the Append/Parse/AppendDiff round-trip + objproto
    # compliance tests entirely (the helpers they exercised are gone).
    # Keep only AppendString tests + ToProto/FromProto round-trip + Equal.
    result = "import (\n"
    result += "    \"strings\"\n"
    result += "    \"testing\"\n"
    result += "    \"math\"\n"
    for imp in c.get("imports", []):
        result += "    \"" + imp + "\"\n"
    result += ")\n\n"
    # ToProto / FromProto round-trip + Equal — Phase β2 typed test.
    for category, metrics in categories.items():
        result += "func Test" + toCamelCase(category) + "StatProtoRoundTrip(t *testing.T) {\n"
        result += "    s := &" + toCamelCase(category) + "Stat{}\n"
        for metric in metrics:
            longName = metric.get("long_name", metric["name"])
            sample_value = gen_sample_value(metric,0)
            result += "    s." + toCamelCase(longName) + " = " + sample_value + "\n"
        result += "    p := s.ToProto()\n"
        result += "    if p == nil { t.Fatalf(\"ToProto returned nil\") }\n"
        result += "    s2 := &" + toCamelCase(category) + "Stat{}\n"
        result += "    s2.FromProto(p)\n"
        result += "    if !s.Equal(s2) {\n"
        result += "        t.Errorf(\"FromProto did not round-trip\")\n"
        result += "    }\n"
        result += "}\n"

    # test AppendString method
    for category, metrics in categories.items():
        result += "func Test" + toCamelCase(category) + "StatAppendString(t *testing.T) {\n"
        result += "    s := &" + toCamelCase(category) + "Stat{}\n"
        # set sample values if provided
        for metric in metrics:
            longName = metric.get("long_name", metric["name"])
            sample_value = gen_sample_value(metric,0)
            result += "    s." + toCamelCase(longName) + " = " + sample_value + "\n"
        result += "    var buf strings.Builder\n"
        result += "    s.AppendString(&buf,\"\")\n"
        result += "    output := buf.String()\n"
        for metric in metrics:
            display_name = metric.get("display_name", metric.get("long_name", metric["name"]))
            result += "    if !strings.Contains(output, \"" + display_name + ": \") {\n"
            result += "        t.Errorf(\"AppendString output missing metric " + display_name + "\")\n"
            result += "    }\n"
        result += " fmt.Printf(\"AppendString output:\\n%s\", output)\n"
        result += "}\n"



    return result

def prometheus_support_methods(statMetrics :dict,categories :dict) -> str:
    result = ""
    prometheus = statMetrics.get("prometheus", [])
    metrics = statMetrics.get("metrics", [])
    prom_converts = statMetrics.get("prometheus_converts", [])
    metrics = {m["name"]:m for m in metrics}
    prom_converts = {p["name"]:p for p in prom_converts}
    mergedInfo = {}
    for prom in prometheus:
        source = metrics[prom["source_metric"]]
        metric_name = prom.get("metric_name", source["name"])
        help_text = prom.get("help", source.get("display_name", source.get("long_name", source["name"])))
        typ = prom["value_type"]
        longName = source.get("long_name", source["name"])
        holdLongName = prom.get("long_name", longName)
        holdType = prom.get("hold_type", source.get("hold_type", source.get("type", "any")))
        to_prom = prom.get("to_prometheus", "")
        category = source["category"]
        labels = prom.get("labels", [])
        optional = prom.get("optional", False)
        optional_condtition = prom.get("optional_condtition", "$src != *new(" + holdType + ")")
        if category not in mergedInfo:
            mergedInfo[category] = {}
        mergedInfo[category][metric_name] = {
            "help": help_text,
            "type": typ,
            "long_name": longName,
            "hold_long_name": holdLongName,
            "hold_type": holdType,
            "labels": labels,
            "source": source,
            "prometheus": prom,
            "to_prometheus": to_prom,
            "optional": optional,
            "optional_condtition": optional_condtition,
        }
    # implement prometheus.Collector interface
    result += "type PromMetrics struct {\n"
    for category, metrics in mergedInfo.items():
        for metric_name, info in metrics.items():
            holdLongName = info["hold_long_name"]
            holdType = info["hold_type"]
            result += "    " + toCamelCase(holdLongName) + " " + holdType + "\n"
    result += "}\n\n"
    for category, metrics in mergedInfo.items():
        result += "func (s *PromMetrics) Update" + toCamelCase(category) + "Stat(stat *" + toCamelCase(category) + "Stat) {\n"
        for metric_name, info in metrics.items():
            longName = info["long_name"]
            holdLongName = info["hold_long_name"]
            holdType = info["hold_type"]
            toProm = info["to_prometheus"]
            if toProm:
                toProm = toProm.replace("$dst", "s." + toCamelCase(holdLongName)).replace("$src", "stat." + toCamelCase(longName))
                result += "    " + toProm + "\n"
                continue
            result += "    s." + toCamelCase(holdLongName) + " = stat." + toCamelCase(longName) + "\n"
        result += "}\n\n"



    result += "func (s *PromMetrics) Collect(ch chan<- prometheus.Metric) {\n"
    for category, metrics in mergedInfo.items():
        for metric_name, info in metrics.items():
            if info["type"] == "label" or info["type"] == "info":
                continue
            holdType = info["hold_type"]
            holdLongName = info["hold_long_name"]
            labelArgs = ",".join([ label.get("convert",prom_converts.get(label["name"],{}).get("convert","")) .replace("$self","s") for label in info["labels"] ])
            if labelArgs:
                labelArgs = "," + labelArgs
            descStr = "prometheus.NewDesc(\"" + metric_name + "\", \"" + info["help"] + "\", []string{"
            descStr += ",".join(['"' + label["name"] + '"' for label in info["labels"]])
            descStr += "}, nil)"
            def convert_type(typ :str) -> str:
                result = ""
                if holdType == "float64":
                    result += "ch <- prometheus.MustNewConstMetric("+ descStr + ", "+typ+", s." + toCamelCase(holdLongName) + labelArgs + ")\n"
                elif "]float" in holdType: # slice of float
                    result += "for i, v := range s." + toCamelCase(holdLongName) + " {\n"
                    result += "    ch <- prometheus.MustNewConstMetric("+ descStr + ", "+typ+", v" + labelArgs + ")\n"
                    result += "}\n"
                elif "]uint" in holdType: # slice of uint
                    result += "for i, v := range s." + toCamelCase(holdLongName) + " {\n"
                    result += "    ch <- prometheus.MustNewConstMetric("+ descStr + ", "+typ+", float64(v)" + labelArgs + ")\n"
                    result += "}\n"
                else:
                    result += "ch <- prometheus.MustNewConstMetric("+ descStr + ", "+typ+", float64(s." + toCamelCase(holdLongName) + ")"+ labelArgs + ")\n"
                return result
            def convert_histogram(typ :str) -> str:
                result = ""
                budgets = info["prometheus"]["map_budgets"].replace("$self","s").replace("$src","s." + toCamelCase(holdLongName))
                sum = info["prometheus"]["map_sum"].replace("$self","s").replace("$src","s." + toCamelCase(holdLongName))
                count = info["prometheus"]["map_count"].replace("$self","s").replace("$src","s." + toCamelCase(holdLongName))
                # source is map[float64]uint64 so we need to iterate it to determine buckets
                result += "ch <- prometheus.MustNewConstHistogram("+ descStr + ", uint64(" + count + "), " + sum + ", "+ budgets + labelArgs  + ")\n"
                return result

            if info["optional"]:
                optional_condtition = info["optional_condtition"].replace("$self","s").replace("$src","s." + toCamelCase(holdLongName))
                result += "    if " + optional_condtition + " {\n"
            if info["type"] == "custom":
                custom_collect = info["prometheus"]["custom_collect"].replace("$desc",descStr).replace("$labels",labelArgs).replace("$self","s").replace("$src","s." + toCamelCase(holdLongName))
                result += "    " + custom_collect + "\n"
            elif info["type"] == "gauge":
                result += convert_type("prometheus.GaugeValue")
            elif info["type"] == "counter":
                result += convert_type("prometheus.CounterValue")
            elif info["type"] == "histogram":
                result += convert_histogram("prometheus.HistogramValue")
            if info["optional"]:
                result += "    }\n"
    result += "}\n\n"
    return result

## ---- proto schema + converter generators (A4 phase 32) ----
#
# Emit protobuf/proto/stat/stat.proto and stat/stat_proto.go so the wire
# format for `stats` admin response can move from ObjectMap (sentinel byte
# 0xFF) to typed AgentControl_AdminResponse{ dto_type:"Stats", body }.
#
# - One proto message per category, mirroring the per-category Stat struct.
# - One top-level Stats envelope with all category sub-messages as
#   optional fields; the broker populates only the ones that are present.
# - ToProto / FromProto helpers on each Stat struct so call sites don't
#   need to know the proto schema details.
#
# Complex Go types map to proto as follows:
#   objproto.ConnectionID, netip.Addr, netip.Prefix → string (canonical)
#   net.HardwareAddr                                 → bytes
#   time.Duration / time.Time                        → int64
#   l4lbdrv.StatCounters, popmetrics.PoPMetrics,
#   dnsmetrics.DNSMetrics ([]byte blob today)        → bytes
#   [3]float64                                       → repeated double
#   []any (hold []net.HardwareAddr)                  → repeated bytes
#   []any (hold []netip.Addr)                        → repeated string
#   []any (hold [][]netip.Prefix)                    → repeated StringList

# Map a hold_type (Go-side type) to a proto field type. Returns
# (proto_type_str, is_repeated). Repeated fields drop the leading 'repeated '
# prefix from the returned type.
def derive_proto_type(metric :dict) -> tuple[str, bool]:
    override = metric.get("proto_type", "")
    if override:
        if override.startswith("repeated "):
            return override[len("repeated "):], True
        return override, False
    holdType = metric.get("hold_type", metric.get("type", "any"))
    # slice / array
    if holdType.startswith("[]") or (holdType.startswith("[") and "]" in holdType):
        # detect [N]T fixed array
        if holdType.startswith("["):
            close_idx = holdType.index("]")
            inner = holdType[close_idx+1:]
        else:
            inner = holdType[2:]
        # Special: []byte → bytes (not repeated uint8)
        if inner == "byte" or inner == "uint8":
            return "bytes", False
        # Wrapper for [][]netip.Prefix etc.
        if inner.startswith("[]"):
            inner_inner = inner[2:]
            if inner_inner in ("netip.Prefix", "netip.Addr", "string"):
                return "StringList", True
        if inner in ("uint32", "net.Flags", "uint16", "uint8"):
            return "uint32", True
        if inner == "uint64":
            return "uint64", True
        if inner in ("int32", "int16", "int8"):
            return "int32", True
        if inner == "int64":
            return "int64", True
        if inner == "float64":
            return "double", True
        if inner == "float32":
            return "float", True
        if inner == "string":
            return "string", True
        if inner == "bool":
            return "bool", True
        if inner == "net.HardwareAddr":
            return "bytes", True
        if inner in ("netip.Addr", "netip.Prefix"):
            return "string", True
        # Fallback: bytes per item
        return "bytes", True
    # scalar
    if holdType in ("uint8", "uint16", "uint32"):
        return "uint32", False
    if holdType == "uint64":
        return "uint64", False
    if holdType in ("int8", "int16", "int32"):
        return "int32", False
    if holdType == "int64":
        return "int64", False
    if holdType == "float64":
        return "double", False
    if holdType == "float32":
        return "float", False
    if holdType == "string":
        return "string", False
    if holdType == "bool":
        return "bool", False
    if holdType in ("time.Duration", "time.Time"):
        return "int64", False
    if holdType == "objproto.ConnectionID":
        return "string", False
    if holdType in ("netip.Addr", "netip.Prefix"):
        return "string", False
    if holdType == "net.HardwareAddr":
        return "bytes", False
    if holdType.startswith("consts."):
        return "uint32", False
    # Binary-blob hold types (l4lbdrv.StatCounters, popmetrics.PoPMetrics,
    # dnsmetrics.DNSMetrics) flow through []byte today.
    return "bytes", False

# For a struct-blob category metric carrying `expand_struct` (a repo-root path to
# the internal_value metrics.json that defines the held struct, e.g.
# dns/dnsmetrics/metrics.json), enumerate the inner fields as typed proto fields
# instead of a single opaque `bytes` envelope. Mirrors internal_value.py's field
# expansion (one field per metric, or one per label for labelled metrics) so the
# emitted proto field snake-names CamelCase back to the same Go names that the
# generated inner struct uses. Returns a list of dicts:
#   {proto_name, go_name, proto_type, hold_type}
# where go_name is the inner struct's Go field and proto_name the proto field.
def expanded_struct_fields(metric :dict) -> list:
    import json
    with open(metric["expand_struct"], "r") as f:
        inner = json.load(f)
    fields = []
    for m in inner.get("metrics", []):
        name = m["name"]
        holdType = m.get("hold_type", m["type"])
        proto_type, _ = derive_proto_type({"hold_type": holdType})
        labels = m.get("labels", [])
        if not labels:
            fields.append({"proto_name": name, "go_name": toCamelCase(name),
                           "proto_type": proto_type, "hold_type": holdType})
        else:
            for label in labels:
                fields.append({"proto_name": name + "_" + label["name"],
                               "go_name": toCamelCase(name) + toCamelCase(label["name"]),
                               "proto_type": proto_type, "hold_type": holdType})
    return fields

# Apply protogen-style proto-name → Go-name conversion: insert an
# uppercase letter break after a digit-letter boundary. The ksdk plugin
# inherits this from protogen.CamelCase, so "L4lbStat" in .proto becomes
# Go type "L4LbStat". Use this when emitting `pbstat.<Name>` references.
def proto_to_go_name(name :str) -> str:
    result = []
    prev_digit = False
    for c in name:
        if prev_digit and c.isalpha() and c.islower():
            result.append(c.upper())
        else:
            result.append(c)
        prev_digit = c.isdigit()
    return "".join(result)

# Emit the protobuf/proto/stat/typed_stats.proto file. Returns the file body.
def process_proto_schema(statMetrics :dict, categories :dict) -> str:
    result = 'syntax = "proto3";\n\n'
    result += 'package ksdk.stat;\n\n'
    result += 'option go_package = "github.com/on-keyday/kscale/protobuf/proto/stat";\n\n'
    result += '// StringList wraps repeated string so it can be used inside\n'
    result += '// "repeated StringList" (proto3 forbids repeated-of-repeated).\n'
    result += 'message StringList {\n'
    result += '    repeated string items = 1;\n'
    result += '}\n\n'
    # one message per category
    for category, metrics in categories.items():
        msg_name = toCamelCase(category) + "Stat"
        result += "message " + msg_name + " {\n"
        field_no = 1
        for metric in metrics:
            # Struct-blob metric flagged for expansion: emit one typed field per
            # inner struct field instead of a single opaque `bytes` envelope.
            if metric.get("expand_struct"):
                for fld in expanded_struct_fields(metric):
                    result += "    " + fld["proto_type"] + " " + fld["proto_name"] + " = " + str(field_no) + ";\n"
                    field_no += 1
                continue
            proto_type, is_repeated = derive_proto_type(metric)
            field_name = metric["name"]
            # proto_presence gives the field explicit presence so partial
            # updates from independent sources don't clobber accumulated state
            # (see derive_presence_converters). Singular scalars become
            # `optional`; repeated-string fields wrap in the nillable StringList
            # message (proto3 repeated has no presence of its own).
            if metric.get("proto_presence"):
                if is_repeated:
                    if proto_type != "string":
                        raise ValueError(
                            "proto_presence on repeated '" + field_name +
                            "': only string elements (StringList) are supported, got " + proto_type
                        )
                    result += "    StringList " + field_name + " = " + str(field_no) + ";\n"
                else:
                    result += "    optional " + proto_type + " " + field_name + " = " + str(field_no) + ";\n"
            else:
                prefix = "repeated " if is_repeated else ""
                result += "    " + prefix + proto_type + " " + field_name + " = " + str(field_no) + ";\n"
            field_no += 1
        result += "}\n\n"
    # Top-level Stats envelope with optional category sub-messages
    result += "// Stats is the wire envelope for the `stats` admin response.\n"
    result += "// Each sub-message is optional; the broker populates only the\n"
    result += "// categories that apply to the dataplane type being reported.\n"
    result += "message Stats {\n"
    # Wire field numbers must never move: a node running an older build keeps
    # sending the old numbers (a renumbered common_name made the CP misread every
    # not-yet-upgraded node). Categories added after common_name/dp_type existed
    # are listed in "appended_categories" and numbered after dp_type, in order.
    field_no = 1
    nested = set(statMetrics.get("nested_categories", []))
    appended = list(statMetrics.get("appended_categories", []))
    for category in categories:
        if category in nested or category in appended:
            continue  # nested-only sub-message, or numbered after dp_type below
        msg_name = toCamelCase(category) + "Stat"
        field_name = category
        result += "    " + msg_name + " " + field_name + " = " + str(field_no) + ";\n"
        field_no += 1
    result += "    // common_name is the reporting node's CA CommonName, self-stamped\n"
    result += "    // by the dp, used to attribute each Stats payload to its source node.\n"
    result += "    // (This is the node identity, NOT a transport connection id.)\n"
    result += "    string common_name = " + str(field_no) + ";\n"
    result += "    // dp_type is the dataplane type (\"l4lb\" / \"popcache\" / etc.).\n"
    result += "    string dp_type = " + str(field_no + 1) + ";\n"
    field_no += 2
    for category in appended:
        if category not in categories:
            raise ValueError("appended_categories: unknown category '" + category + "'")
        result += "    " + toCamelCase(category) + "Stat " + category + " = " + str(field_no) + ";\n"
        field_no += 1
    result += "}\n\n"
    return result

# Presence-aware ToProto / FromProto for fields flagged proto_presence.
# ToProto always populates (GetStat composes a full snapshot, so every field
# is meaningfully set). FromProto applies a field only when the wire carried
# it (non-nil pointer / non-nil StringList), so a partial update from one
# source does not zero out fields owned by another source.
def derive_presence_converters(metric :dict, holdType :str, src :str, dst :str, in_field :str, proto_type :str, is_repeated :bool) -> tuple[str, str]:
    if not is_repeated:
        # singular scalar → *proto_type pointer
        # (to_val: domain→proto value; from_expr: proto(*ptr)→domain value)
        if holdType in ("uint8", "uint16", "int8", "int16"):
            to_val, from_expr = f"{proto_type}({src})", f"{holdType}(*{in_field})"
        elif holdType in ("uint32", "uint64", "int32", "int64", "float64", "float32", "string", "bool"):
            to_val, from_expr = src, f"*{in_field}"
        elif holdType == "time.Duration":
            to_val, from_expr = f"int64({src})", f"time.Duration(*{in_field})"
        elif holdType == "time.Time":
            to_val, from_expr = f"{src}.UnixNano()", f"time.Unix(0, *{in_field})"
        elif holdType.startswith("consts."):
            to_val, from_expr = f"uint32({src})", f"{holdType}(*{in_field})"
        else:
            raise ValueError("proto_presence on scalar hold_type '" + holdType + "' is not supported")
        to = "{\n"
        to += f"        tmp := {to_val}\n"
        to += f"        {dst} = &tmp\n"
        to += "    }\n"
        frm = f"if {in_field} != nil {{ {src} = {from_expr} }}\n"
        return to, frm
    # repeated → StringList wrapper (string elements only)
    if proto_type != "string":
        raise ValueError("proto_presence on repeated proto_type '" + proto_type + "' is not supported")
    inner = holdType[2:]
    if inner == "string":
        to = f"{dst} = &pbstat.StringList{{Items: append([]string(nil), {src}...)}}\n"
        frm = f"if {in_field} != nil {{ {src} = append([]string(nil), {in_field}.Items...) }}\n"
        return to, frm
    if inner in ("netip.Addr", "netip.Prefix"):
        parse = "netip.ParseAddr" if inner == "netip.Addr" else "netip.ParsePrefix"
        to = "{\n"
        to += f"        items := make([]string, len({src}))\n"
        to += f"        for i, v := range {src} {{ items[i] = v.String() }}\n"
        to += f"        {dst} = &pbstat.StringList{{Items: items}}\n"
        to += "    }\n"
        frm = f"if {in_field} != nil {{\n"
        frm += f"        tmp := make([]{inner}, len({in_field}.Items))\n"
        frm += f"        for i, v := range {in_field}.Items {{ tmp[i], _ = {parse}(v) }}\n"
        frm += f"        {src} = tmp\n"
        frm += "    }\n"
        return to, frm
    raise ValueError("proto_presence on repeated element type '" + inner + "' is not supported")

# Per-metric ToProto / FromProto fragments. Returns (to_lines, from_lines).
def derive_proto_converters(metric :dict) -> tuple[str, str]:
    longName = metric.get("long_name", metric["name"])
    holdType = metric.get("hold_type", metric.get("type", "any"))
    fieldName = proto_to_go_name(toCamelCase(metric["name"]))  # proto field Go name
    structField = toCamelCase(longName)      # Go Stat struct field name
    src = "s." + structField
    dst = "out." + fieldName
    proto_type, is_repeated = derive_proto_type(metric)
    # Struct-blob metric flagged for expansion: copy each inner struct field into
    # its own typed proto field (and back) instead of a single binary.Encode blob.
    if metric.get("expand_struct"):
        structField = toCamelCase(longName)  # held struct: s.DnsStats / s.PopcacheStats
        to = ""
        frm = ""
        for fld in expanded_struct_fields(metric):
            pgo = proto_to_go_name(toCamelCase(fld["proto_name"]))  # proto field Go name
            d = "out." + pgo
            sfield = "s." + structField + "." + fld["go_name"]
            inf = "in." + pgo
            ht = fld["hold_type"]
            if ht in ("uint8", "uint16"):
                to += f"{d} = uint32({sfield})\n"; frm += f"{sfield} = {ht}({inf})\n"
            elif ht in ("int8", "int16"):
                to += f"{d} = int32({sfield})\n"; frm += f"{sfield} = {ht}({inf})\n"
            else:  # uint32 / uint64 / int32 / int64 — proto type matches, direct copy
                to += f"{d} = {sfield}\n"; frm += f"{sfield} = {inf}\n"
        return to, frm
    # custom override
    if "to_proto" in metric or "from_proto" in metric:
        to_lines = metric.get("to_proto", "").replace("$dst", dst).replace("$src", src)
        from_lines = metric.get("from_proto", "").replace("$dst", src).replace("$src", "in." + fieldName)
        return to_lines + "\n", from_lines + "\n"
    # By hold_type, choose conversion
    in_field = "in." + fieldName
    # Presence fields use pointer (scalar) / StringList (repeated string)
    # wire types, so ToProto always populates (full snapshot) and FromProto
    # merges only when the wire field is present.
    if metric.get("proto_presence"):
        return derive_presence_converters(metric, holdType, src, dst, in_field, proto_type, is_repeated)
    # scalar primitives
    if not is_repeated:
        if holdType in ("uint8", "uint16"):
            return f"{dst} = uint32({src})\n", f"{src} = {holdType}({in_field})\n"
        if holdType in ("uint32", "uint64", "int32", "int64", "float64", "float32", "string", "bool"):
            return f"{dst} = {src}\n", f"{src} = {in_field}\n"
        if holdType in ("int8", "int16"):
            return f"{dst} = int32({src})\n", f"{src} = {holdType}({in_field})\n"
        if holdType in ("time.Duration", "time.Time"):
            if holdType == "time.Duration":
                return f"{dst} = int64({src})\n", f"{src} = time.Duration({in_field})\n"
            else:
                return f"{dst} = {src}.UnixNano()\n", f"{src} = time.Unix(0, {in_field})\n"
        if holdType == "objproto.ConnectionID":
            return f"{dst} = {src}.String()\n", (
                f"if v, err := objproto.ParseConnectionID({in_field}, 0); err == nil {{ {src} = v }}\n"
            )
        if holdType == "netip.Addr":
            return f"{dst} = {src}.String()\n", (
                f"if v, err := netip.ParseAddr({in_field}); err == nil {{ {src} = v }}\n"
            )
        if holdType == "netip.Prefix":
            return f"{dst} = {src}.String()\n", (
                f"if v, err := netip.ParsePrefix({in_field}); err == nil {{ {src} = v }}\n"
            )
        if holdType == "net.HardwareAddr":
            return f"{dst} = []byte({src})\n", f"{src} = net.HardwareAddr({in_field})\n"
        if holdType.startswith("consts."):
            return f"{dst} = uint32({src})\n", f"{src} = {holdType}({in_field})\n"
        # binary-blob hold types (l4lbdrv.StatCounters etc.)
        if holdType not in ("string", "bool"):
            to = "{\n"
            to += f"        var w [unsafe.Sizeof({holdType}{{}})]byte\n"
            to += f"        binary.Encode(w[:], binary.LittleEndian, {src})\n"
            to += f"        {dst} = w[:]\n"
            to += "    }\n"
            frm = "{\n"
            frm += f"        var res {holdType}\n"
            frm += f"        binary.Read(bytes.NewReader({in_field}), binary.LittleEndian, &res)\n"
            frm += f"        {src} = res\n"
            frm += "    }\n"
            return to, frm
    # repeated / slice
    # fixed array [N]T (e.g. [3]float64) — copy slice both directions
    if holdType.startswith("[") and not holdType.startswith("[]"):
        close_idx = holdType.index("]")
        size = holdType[1:close_idx]
        inner = holdType[close_idx+1:]
        to = f"{dst} = append([]{inner}(nil), {src}[:]...)\n"
        frm = "{\n"
        frm += f"        var arr [{size}]{inner}\n"
        frm += f"        copy(arr[:], {in_field})\n"
        frm += f"        {src} = arr\n"
        frm += "    }\n"
        return to, frm
    # []T slice
    inner = holdType[2:]
    if inner in ("uint32", "uint64", "int32", "int64", "float64", "float32", "string", "bool"):
        return (
            f"{dst} = append([]{inner}(nil), {src}...)\n",
            f"{src} = append([]{inner}(nil), {in_field}...)\n",
        )
    if inner in ("uint8", "uint16", "int8", "int16"):
        proto_inner = "uint32" if inner.startswith("u") else "int32"
        to = "{\n"
        to += f"        tmp := make([]{proto_inner}, len({src}))\n"
        to += f"        for i, v := range {src} {{ tmp[i] = {proto_inner}(v) }}\n"
        to += f"        {dst} = tmp\n"
        to += "    }\n"
        frm = "{\n"
        frm += f"        tmp := make([]{inner}, len({in_field}))\n"
        frm += f"        for i, v := range {in_field} {{ tmp[i] = {inner}(v) }}\n"
        frm += f"        {src} = tmp\n"
        frm += "    }\n"
        return to, frm
    if inner == "net.Flags":
        to = "{\n"
        to += f"        tmp := make([]uint32, len({src}))\n"
        to += f"        for i, v := range {src} {{ tmp[i] = uint32(v) }}\n"
        to += f"        {dst} = tmp\n"
        to += "    }\n"
        frm = "{\n"
        frm += f"        tmp := make([]net.Flags, len({in_field}))\n"
        frm += f"        for i, v := range {in_field} {{ tmp[i] = net.Flags(v) }}\n"
        frm += f"        {src} = tmp\n"
        frm += "    }\n"
        return to, frm
    if inner == "net.HardwareAddr":
        to = "{\n"
        to += f"        tmp := make([][]byte, len({src}))\n"
        to += f"        for i, v := range {src} {{ tmp[i] = []byte(v) }}\n"
        to += f"        {dst} = tmp\n"
        to += "    }\n"
        frm = "{\n"
        frm += f"        tmp := make([]net.HardwareAddr, len({in_field}))\n"
        frm += f"        for i, v := range {in_field} {{ tmp[i] = net.HardwareAddr(v) }}\n"
        frm += f"        {src} = tmp\n"
        frm += "    }\n"
        return to, frm
    if inner == "netip.Addr":
        to = "{\n"
        to += f"        tmp := make([]string, len({src}))\n"
        to += f"        for i, v := range {src} {{ tmp[i] = v.String() }}\n"
        to += f"        {dst} = tmp\n"
        to += "    }\n"
        frm = "{\n"
        frm += f"        tmp := make([]netip.Addr, len({in_field}))\n"
        frm += f"        for i, v := range {in_field} {{ tmp[i], _ = netip.ParseAddr(v) }}\n"
        frm += f"        {src} = tmp\n"
        frm += "    }\n"
        return to, frm
    if inner == "netip.Prefix":
        to = "{\n"
        to += f"        tmp := make([]string, len({src}))\n"
        to += f"        for i, v := range {src} {{ tmp[i] = v.String() }}\n"
        to += f"        {dst} = tmp\n"
        to += "    }\n"
        frm = "{\n"
        frm += f"        tmp := make([]netip.Prefix, len({in_field}))\n"
        frm += f"        for i, v := range {in_field} {{ tmp[i], _ = netip.ParsePrefix(v) }}\n"
        frm += f"        {src} = tmp\n"
        frm += "    }\n"
        return to, frm
    if inner.startswith("[]"):
        inner_inner = inner[2:]
        if inner_inner == "netip.Prefix":
            to = "{\n"
            to += f"        tmp := make([]*pbstat.StringList, len({src}))\n"
            to += f"        for i, sub := range {src} {{\n"
            to += "            items := make([]string, len(sub))\n"
            to += "            for j, v := range sub { items[j] = v.String() }\n"
            to += "            tmp[i] = &pbstat.StringList{ Items: items }\n"
            to += "        }\n"
            to += f"        {dst} = tmp\n"
            to += "    }\n"
            frm = "{\n"
            frm += f"        tmp := make([][]netip.Prefix, len({in_field}))\n"
            frm += f"        for i, sub := range {in_field} {{\n"
            frm += "            if sub == nil { continue }\n"
            frm += "            items := make([]netip.Prefix, len(sub.Items))\n"
            frm += "            for j, v := range sub.Items { items[j], _ = netip.ParsePrefix(v) }\n"
            frm += "            tmp[i] = items\n"
            frm += "        }\n"
            frm += f"        {src} = tmp\n"
            frm += "    }\n"
            return to, frm
    # Fallback: per-element bytes
    return (
        f"// TODO: unsupported slice element type {inner} for {longName}\n",
        f"// TODO: unsupported slice element type {inner} for {longName}\n",
    )

# Emit stat/stat_proto.go: ToProto/FromProto per category + Stat envelope.
def process_proto_converters(statMetrics :dict, categories :dict) -> str:
    result = 'package stat\n\n'
    # imports — we let goimports finalize, but emit the obvious ones so the
    # file compiles before goimports runs.
    result += 'import (\n'
    result += '    "bytes"\n'
    result += '    "encoding/binary"\n'
    result += '    "net"\n'
    result += '    "net/netip"\n'
    result += '    "time"\n'
    result += '    "unsafe"\n\n'
    result += '    "github.com/on-keyday/kscale/consts"\n'
    result += '    "github.com/on-keyday/kscale/dns/dnsmetrics"\n'
    result += '    "github.com/on-keyday/kscale/l4lb/l4lbdrv"\n'
    result += '    "github.com/on-keyday/objtrsf/objproto"\n'
    result += '    "github.com/on-keyday/kscale/popcache/popmetrics"\n'
    result += '    pbstat "github.com/on-keyday/kscale/protobuf/proto/stat"\n'
    result += ')\n\n'
    # Suppress "imported and not used" if any category happens to drop a
    # type — reference them in a no-op init.
    result += 'var _ = bytes.NewReader\n'
    result += 'var _ = binary.LittleEndian\n'
    result += 'var _ = net.HardwareAddr(nil)\n'
    result += 'var _ = netip.Addr{}\n'
    result += 'var _ = time.Duration(0)\n'
    result += 'var _ = unsafe.Sizeof(byte(0))\n'
    result += 'var _ = consts.AppStatus(0)\n'
    result += 'var _ = dnsmetrics.DNSMetrics{}\n'
    result += 'var _ = l4lbdrv.StatCounters{}\n'
    result += 'var _ = popmetrics.PoPMetrics{}\n'
    result += 'var _ = objproto.ConnectionID{}\n\n'
    for category, metrics in categories.items():
        cat_camel = toCamelCase(category)
        go_struct = cat_camel + "Stat"
        # `pbstat.<Name>` mirrors what protoc-gen-ksdk emits, which applies
        # protogen-style digit-letter CamelCase. e.g. proto "L4lbStat" →
        # Go type "L4LbStat".
        proto_struct = "pbstat." + proto_to_go_name(cat_camel) + "Stat"
        # ToProto
        result += f"func (s *{go_struct}) ToProto() *{proto_struct} {{\n"
        result += f"    out := &{proto_struct}{{}}\n"
        for metric in metrics:
            to_lines, _ = derive_proto_converters(metric)
            for line in to_lines.splitlines():
                result += "    " + line + "\n"
        result += "    return out\n"
        result += "}\n\n"
        # FromProto
        result += f"func (s *{go_struct}) FromProto(in *{proto_struct}) {{\n"
        result += "    if in == nil { return }\n"
        for metric in metrics:
            _, frm_lines = derive_proto_converters(metric)
            for line in frm_lines.splitlines():
                result += "    " + line + "\n"
        result += "}\n\n"
    return result


def loadAndMergeStatMetrics(statMetricsDir :str) -> dict:
    import os
    import json
    import yaml
    statMetrics = {
        "imports": [],
        "metrics": [],
        "prometheus": [],
        "prometheus_converts": [],
        # Categories that exist only as nested sub-messages (e.g. interface_spec,
        # held by network_spec as `repeated InterfaceSpecStat`) — emitted as their
        # own message + struct + ToProto, but kept OUT of the top-level Stats
        # envelope (the broker never reports them as a standalone category).
        "nested_categories": [],
        # Top-level categories numbered after common_name/dp_type (wire field
        # numbers are append-only; see process_proto_schema).
        "appended_categories": [],
    }
    # Sort filenames so the generated output is stable across machines /
    # Python versions (os.listdir order is FS-dependent).
    for filename in sorted(os.listdir(statMetricsDir)):
        if not filename.endswith(".json") and not filename.endswith(".yaml") and not filename.endswith(".yml"):
            continue
        filepath = os.path.join(statMetricsDir, filename)
        with open(filepath, 'r') as f:
            if filename.endswith(".yaml") or filename.endswith(".yml"):
                data = yaml.safe_load(f)
            else:
                data = json.load(f)
            for key in ["imports", "metrics", "prometheus", "prometheus_converts", "nested_categories", "appended_categories"]:
                if key in data:
                    statMetrics[key].extend(data[key])
    return statMetrics

if __name__ == "__main__":
    import sys
    statMetricsDir = sys.argv[1]
    statMetrics = loadAndMergeStatMetrics(statMetricsDir)
    STAT_OUTPUT = "stat/stat_metrics.go"
    stat_output,categories = process_stat_metrics(statMetrics)
    stat_output = "// Code generated by script/internal_value.py. DO NOT EDIT.\n\npackage stat\n\n" + stat_output
    with open(STAT_OUTPUT, 'w') as f:
        f.write(stat_output)
    STAT_TEST_OUTPUT = "stat/stat_metrics_test.go"
    stat_test_output= objproto_compliance_test(statMetrics,categories)
    stat_test_output = "// Code generated by script/internal_value.py. DO NOT EDIT.\n\npackage stat\n\n" + stat_test_output
    with open(STAT_TEST_OUTPUT, 'w') as f:
        f.write(stat_test_output)
    PROM_SUPPORT_OUTPUT = "stat/prometheus_metrics.go"
    prom_support_output = prometheus_support_methods(statMetrics,categories)
    prom_support_output = "// Code generated by script/internal_value.py. DO NOT EDIT.\n\npackage stat\n\nimport (\n    \"github.com/prometheus/client_golang/prometheus\"\n)\n\n" + prom_support_output
    with open(PROM_SUPPORT_OUTPUT, 'w') as f:
        f.write(prom_support_output)
    # A4 phase 32: proto schema + Go converters for typed stats wire path.
    # NOTE: this writes to typed_stats.proto, NOT stat.proto. stat.proto
    # already exists with the stat streaming envelope types
    # (StatStreamHeader / StatBatch / StatReport) from commit 8a9f60e.
    import os
    PROTO_OUTPUT = "protobuf/proto/stat/typed_stats.proto"
    os.makedirs(os.path.dirname(PROTO_OUTPUT), exist_ok=True)
    proto_schema = process_proto_schema(statMetrics, categories)
    proto_schema = "// Code generated by script/metrics.py. DO NOT EDIT.\n\n" + proto_schema
    with open(PROTO_OUTPUT, 'w') as f:
        f.write(proto_schema)
    PROTO_CONVERTER_OUTPUT = "stat/stat_proto.go"
    proto_converters = process_proto_converters(statMetrics, categories)
    proto_converters = "// Code generated by script/metrics.py. DO NOT EDIT.\n\n" + proto_converters
    with open(PROTO_CONVERTER_OUTPUT, 'w') as f:
        f.write(proto_converters)
    # run go fmt
    import subprocess
    subprocess.run(["go", "fmt", STAT_OUTPUT, STAT_TEST_OUTPUT, PROM_SUPPORT_OUTPUT, PROTO_CONVERTER_OUTPUT])
    # do goimports (lookup from GOPATH)
    go_path = subprocess.check_output(["go", "env", "GOPATH"]).decode().strip()
    abs_goimports = go_path + "/bin/goimports"
    subprocess.run([abs_goimports, "-w", STAT_OUTPUT, STAT_TEST_OUTPUT, PROM_SUPPORT_OUTPUT, PROTO_CONVERTER_OUTPUT])