package policy

import (
	"net/netip"
	"regexp"
)

// helpers.go (生成コードから呼ばれる関数群)

func regexMatch(target string, pattern *regexp.Regexp) bool {
	// 実際の実装ではコンパイル済み正規表現をキャッシュすべきですが、簡易実装としてはこれ
	return pattern.MatchString(target)
}

func stringListContainsAny(target []string, candidates map[string]struct{}) bool {
	// マップを使ったO(N)実装が望ましいが、簡易実装
	for _, t := range target {
		if _, found := candidates[t]; found {
			return true
		}
	}
	return false
}

func stringListContainsAll(target, required []string) bool {
	for _, req := range required {
		found := false
		for _, t := range target {
			if t == req {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func ipEquals(ip1 string, ip2 netip.Addr) (bool, error) {
	parsedIP1, err := netip.ParseAddr(ip1)
	if err != nil {
		return false, err
	}
	return parsedIP1 == ip2, nil
}

func ipInCidr(ipStr string, cidrStr netip.Prefix) (bool, error) {
	ip, err := netip.ParseAddr(ipStr)
	if err != nil {
		return false, err
	}
	return cidrStr.Contains(ip), nil
}

func makeStringSet(list []string) (map[string]struct{}, error) {
	set := make(map[string]struct{}, len(list))
	for _, item := range list {
		set[item] = struct{}{}
	}
	return set, nil
}
