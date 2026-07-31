import json
import os
import sys

configsFile = sys.argv[1]
appNameFile = sys.argv[2]
streamMagicFile = sys.argv[3]
magicNumbersFile = sys.argv[4]
appStatusFile = sys.argv[5]
genericControlFile = sys.argv[6]
popMetricsFile = sys.argv[7]
dnsMetricsFile = sys.argv[8]

with open(configsFile, 'r') as f:
    configs = json.load(f)

with open(appNameFile, 'r') as f:
    appNames = json.load(f)


with open(streamMagicFile, 'r') as f:
    streamMagic = json.load(f)

with open(magicNumbersFile, 'r') as f:
    magicNumbers = json.load(f)

with open(appStatusFile, 'r') as f:
    appStatus = json.load(f)

with open(genericControlFile, 'r') as f:
    genericControl = json.load(f)

with open(popMetricsFile, 'r') as f:
    popMetrics = json.load(f)

with open(dnsMetricsFile, 'r') as f:
    dnsMetrics = json.load(f)

def collect_imports(configs):
    imports = set()
    for config in configs.get("config", []):
        imports.update(config.get("imports", []))
    return imports

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

def process_config(configs):
    result = ""
    for config in configs.get("config", []):
        result += "const ConfigKey" + str(config["name"]) + " = \"" + str(config["name"]) + "\"\n\n"
        result += "func Get" + str(config["name"]) + "(conf map[string]any) (" + str(config["type"]) + ", bool)  {\n"
        result += "    v, ok := conf[\"" + str(config["name"]) + "\"].(" + str(config["type"]) + ")\n"
        result += "    return v, ok\n"
        result += "}\n\n"
        result += "func Set" + str(config["name"]) + "(value " + str(config["type"]) + ") agent.ConfigField {\n"
        result += "    return agent.ConfigField{\n"
        result += "        Key: ConfigKey" + str(config["name"]) + ",\n"
        result += "        Value: value,\n"
        result += "    }\n"
        result += "}\n\n"
    return result

def process_app_names(appNames):
    result = ""
    for app in appNames.get("apps", []):
        result += "const AppName" + toCamelCase(app["name"]) + " = \"" + str(app["name"]) + "\"\n\n"
    return result



def process_stream_magic(streamMagic):
    result = ""
    for magic in streamMagic.get("magic", []):
        magicName = magic["magic"]
        desc = magic.get("description", "")
        result += "// " + desc + "\n" if desc else ""
        result += "const StreamMagic" + toCamelCase(magicName) + " = \"" + str(magicName) + "\"\n\n"
    return result

def process_magic_numbers(magicNumbers):
    result = ""
    for number in magicNumbers.get("numbers", []):
        numberName = number["name"]
        value = number["value"]
        desc = number.get("description", "")
        result += "// " + desc + "\n" if desc else ""
        result += "const MagicNumber" + toCamelCase(numberName) + " = " + str(value) + "\n\n"
    return result

def process_app_status(appStatus):
    result = "import \"fmt\"\n\n"
    result += "type AppStatus uint8\n"
    for status in appStatus.get("status", []):
        statusName = status["name"]
        value = status["value"]
        desc = status.get("description", "")
        result += "// " + desc + "\n" if desc else ""
        result += "const AppStatus" + toCamelCase(statusName) + " AppStatus = " + str(value) + "\n\n"
    result += "func (s AppStatus) String() string {\n"
    result += "    switch s {\n"
    for status in appStatus.get("status", []):
        statusName = status["name"]
        result += "    case AppStatus" + toCamelCase(statusName) + ":\n"
        result += "        return \"" + str(statusName) + "\"\n"
    result += "    default:\n"
    result += "        return fmt.Sprintf(\"AppStatus(%d)\", s)\n"
    result += "    }\n"
    result += "}\n\n"
    return result

def process_generic_control(genericControl):
    result = ""
    for control in genericControl.get("controls", []):
        controlName = control["name"]
        result += "const GenericControl" + toCamelCase(controlName) + " = \"" + str(controlName) + "\"\n\n"
    return result

def process_dataplane_metrics(popMetrics,strucName):
    result = "import (\n"
    result += '    "fmt"\n'
    result += '    "strings"\n'
    result += '    "sync/atomic"\n'
    result += '    "sync"\n'
    result += ")\n\n"
    result += "type " + strucName + " struct {\n"
    for metric in popMetrics.get("metrics", []):
        metricName = metric["name"]
        metricType = metric.get("hold_type", metric["type"])
        labels = metric.get("labels", [])
        if len(labels) == 0:
            result += "    " + toCamelCase(metricName) + " " + metricType + "\n"
        else:
            for label in labels:
                result += "    " + toCamelCase(metricName) + toCamelCase(label["name"]) + " " + metricType + "\n"
    result += "}\n\n"
    # with RWMutex
    result += "type " + strucName + "WithLock struct {\n"
    result += "    metricsLock sync.RWMutex\n"
    result += "    metrics     " + strucName + "\n"
    result += "}\n\n"
    interfaceImpl = "type " + strucName + "MetricsInterface interface {\n"
    for metric in popMetrics.get("metrics", []):
        metricName = metric["name"]
        setterType = metric["type"]
        holdType = metric.get("hold_type", metric["type"])
        metricKind = metric.get("kind", "counter")
        labels = metric.get("labels", [])
        # increment atomically
        if metricKind == "counter":
            def generateAdd(metric_name :str):
                nonlocal result
                nonlocal interfaceImpl
                result += "func (p *" + strucName + ") Add" + metric_name + "(v " + setterType + ") {\n"
                result += "    atomic.Add" + toCamelCase(holdType) + "(&p." + metric_name + ", " + holdType + "(v))\n"
                result += "}\n\n"
                # Increment for Add(1)
                result += "func (p *" + strucName + ") Increment" + metric_name + "() {\n"
                result += "    p.Add"+ metric_name + "(1)\n"
                result += "}\n\n"
                result += "func (p *"+ strucName +"WithLock) Add" + metric_name + "(v " + setterType + ") {\n"
                result += "    p.metricsLock.RLock()\n"
                result += "    defer p.metricsLock.RUnlock()\n"
                result += "    p.metrics.Add" + metric_name + "(v)\n"
                result += "}\n\n"
                result += "func (p *"+ strucName +"WithLock) Increment" + metric_name + "() {\n"
                result += "    p.metricsLock.RLock()\n"
                result += "    defer p.metricsLock.RUnlock()\n"
                result += "    p.metrics.Increment" + metric_name + "()\n"
                result += "}\n\n"
                interfaceImpl += "    Add" + metric_name + "(v " + setterType + ")\n"
                interfaceImpl += "    Increment" + metric_name + "()\n"
            if len(labels) == 0:
                generateAdd(toCamelCase(metricName))
            else:
                for label in labels:
                    generateAdd(toCamelCase(metricName) + toCamelCase(label["name"]))
        elif metricKind == "gauge":
            result += "func (p *" + strucName + ") Set" + toCamelCase(metricName) + "(v " + setterType + ") {\n"
            result += "    atomic.Store" + toCamelCase(holdType) + "(&p." + toCamelCase(metricName) + ", " + holdType + "(v))\n"
            result += "}\n\n"
            result += "func (p *"+ strucName +"WithLock) Set" + toCamelCase(metricName) + "(v " + setterType + ") {\n"
            result += "    p.metricsLock.RLock()\n"
            result += "    defer p.metricsLock.RUnlock()\n"
            result += "    p.metrics.Set" + toCamelCase(metricName) + "(v)\n"
            result += "}\n\n"
            interfaceImpl += "    Set" + toCamelCase(metricName) + "(v " + setterType + ")\n"
    interfaceImpl += "}\n\n"
    result += interfaceImpl
    result += "func (p *" + strucName + ") String() string {\n"
    result += "    var buf strings.Builder\n"
    result += "    buf.WriteString(\"" + strucName + "{\")\n"
    for metric in popMetrics.get("metrics", []):
        metricName = metric["name"]
        labels = metric.get("labels", [])
        def process_metric(metric_name :str):
            nonlocal result
            result += ' if p.' + metric_name + ' != 0 {\n'
            result += '    if buf.Len() > len("' + strucName + '{") { buf.WriteString(", ") }\n'
            result += '    buf.WriteString(fmt.Sprintf("' + metric_name + ': %v", p.' + metric_name + '))\n'
            result += " }\n"
        if len(labels) == 0:
            process_metric(toCamelCase(metricName))
        else:
            for label in labels:
                process_metric(toCamelCase(metricName) + toCamelCase(label["name"]))
    result += "    buf.WriteString(\"}\")\n"
    result += "    return buf.String()\n"
    result += "}\n\n"
    # get with lock
    result += "func (p *" + strucName + "WithLock) GetMetrics() " + strucName + " {\n"
    result += "    p.metricsLock.Lock()\n" 
    result += "    defer p.metricsLock.Unlock()\n"
    result += "    return p.metrics\n"
    result += "}\n\n"
    result += "func (p *" + strucName + "WithLock) WithLock(cb func(m *" + strucName + ")) {\n"
    result += "    p.metricsLock.Lock()\n" 
    result += "    defer p.metricsLock.Unlock()\n"
    result += "    cb(&p.metrics)\n"
    result += "}\n\n"
    return result

def process_dataplane_metrics_json(popMetrics,metricBase,srcMetric,prefix):
    result = []
    for metric in popMetrics.get("metrics", []):
        holdType = metric["type"]
        labels = metric.get("labels", [])
        toPrometheus = "$dst = " + metric["type"] + "($src." + toCamelCase(metric["name"]) + ")"
        metricLabels = [{"name":"instance"}]
        if len(labels) > 0:
            holdType = "["+str(len(labels))+"]"+ holdType
            toPrometheus = "$dst = " + holdType + "{" + ", ".join([metric["type"] + "($src." + toCamelCase(metric["name"]) + toCamelCase(label["name"]) + ")" for label in labels]) + "}"
            metricLabels += [{"name": metric["label_name"],"convert": "func() string { switch i {" + "\n".join(["case " + str(idx) + ": \n return \"" + label["label"] + "\"" for idx, label in enumerate(labels)]) + "\n default: return \"unknown\" } }()"}]
        base = {
            "metric_name": "ksdk_"+ metricBase +"_" + metric["name"],
            "help": metric.get("help", metricBase.capitalize() + " metric " + metric["name"]),
            "long_name": prefix + toCamelCase(metric["name"]),
            "value_type": metric.get("kind", "counter"),
            "hold_type": holdType,
            "source_metric": srcMetric,
            "optional": True,
            "optional_condtition": "$self."+ prefix + "Enabled",
            "to_prometheus": toPrometheus,
            "labels": metricLabels,
        }
        result.append(base)
    return json.dumps({"prometheus": result}, indent=4)


CONFIG_OUTPUT = "config/config.go"
STAT_OUTPUT = "stat/stat_metrics.go"
STREAM_OUTPUT = "consts/stream_magic.go"
MAGIC_NUMBERS_OUTPUT = "consts/magic_numbers.go"
APP_STATUS_OUTPUT = "consts/app_status.go"
GENERIC_CONTROL_OUTPUT = "consts/generic_control.go"

dpMetrics = [
    {
        "output": "popcache/popmetrics/popcache_metrics.go",
        "json_output": "access/defs/internal/stats/popcache_metrics.json",
        "struct_name": "PoPMetrics",
        "packege": "popmetrics",
        "metric_base": "popcache",
        "source_metric": "popcache_stats",
        "prefix": "Popcache",
        "metrics": popMetrics
    },
    {
        "output": "dns/dnsmetrics/dns_metrics.go",
        "json_output": "access/defs/internal/stats/dns_metrics.json",
        "struct_name": "DNSMetrics",
        "packege": "dnsmetrics",
        "metric_base": "dns",
        "source_metric": "dns_stats",
        "prefix": "Dns",
        "metrics": dnsMetrics
    }
]
if __name__ == "__main__":
    imports = collect_imports(configs)
    output = process_config(configs)
    output += process_app_names(appNames)
    if imports:
        import_lines = "import (\n"
        for imp in sorted(imports):
            import_lines += f'    "{imp}"\n'
        import_lines += ")\n\n"
        output = import_lines + output
    output = "// Code generated by script/internal_value.py. DO NOT EDIT.\n\npackage config\n\n" + output
    # kscale deviation: only emit config/config.go when a config package dir
    # already exists. ksdk has a config package; kscale (control-plane rebuild)
    # does not yet, and config_key.json references a kscale/agent package that
    # has not been salvaged. Generating config.go unconditionally would create a
    # non-building package. The metrics/stat outputs below (consts, popmetrics,
    # dnsmetrics) are unaffected.
    if os.path.isdir(os.path.dirname(CONFIG_OUTPUT)):
        with open(CONFIG_OUTPUT, 'w') as f:
            f.write(output)
    stream_output = process_stream_magic(streamMagic)
    stream_output = "// Code generated by script/internal_value.py. DO NOT EDIT.\n\npackage consts\n\n" + stream_output
    with open(STREAM_OUTPUT, 'w') as f:
        f.write(stream_output)
    magic_numbers_output = process_magic_numbers(magicNumbers)
    magic_numbers_output = "// Code generated by script/internal_value.py. DO NOT EDIT.\n\npackage consts\n\n" + magic_numbers_output
    with open(MAGIC_NUMBERS_OUTPUT, 'w') as f:
        f.write(magic_numbers_output)
    app_status_output = process_app_status(appStatus)
    app_status_output = "// Code generated by script/internal_value.py. DO NOT EDIT.\n\npackage consts\n\n" + app_status_output
    with open(APP_STATUS_OUTPUT, 'w') as f:
        f.write(app_status_output)
    generic_control_output = process_generic_control(genericControl)
    generic_control_output = "// Code generated by script/internal_value.py. DO NOT EDIT.\n\npackage consts\n\n" + generic_control_output
    with open(GENERIC_CONTROL_OUTPUT, 'w') as f:
        f.write(generic_control_output)
    for dpMetric in dpMetrics:
        dp_metric_output = process_dataplane_metrics(dpMetric["metrics"], dpMetric["struct_name"])
        dp_metric_output = "// Code generated by script/internal_value.py. DO NOT EDIT.\n\npackage " + dpMetric["packege"] + "\n\n" + dp_metric_output
        with open(dpMetric["output"], 'w') as f:
            f.write(dp_metric_output)
        dp_metric_json_output = process_dataplane_metrics_json(
            dpMetric["metrics"],
            dpMetric["metric_base"],
            dpMetric["source_metric"],
            dpMetric["prefix"]
        )
        with open(dpMetric["json_output"], 'w') as f:
            f.write(dp_metric_json_output)
