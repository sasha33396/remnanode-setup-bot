package abuse

import (
	"bufio"
	"errors"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

type NetworkRecord struct {
	Prefix       netip.Prefix
	Intelligence Intelligence
}

type LocalIntelligence struct{ records []NetworkRecord }

func LoadLocalIntelligence(path string, categoryOverrides map[uint32]string) (*LocalIntelligence, error) {
	db := &LocalIntelligence{}
	if strings.TrimSpace(path) == "" {
		return db, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("open local IP intelligence database failed")
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 2 {
			return nil, errors.New("invalid local IP intelligence row")
		}
		prefix, prefixErr := netip.ParsePrefix(strings.TrimSpace(parts[0]))
		asnText := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(parts[1])), "AS")
		asn, asnErr := strconv.ParseUint(asnText, 10, 32)
		if prefixErr != nil || asnErr != nil {
			return nil, errors.New("invalid local IP intelligence network or ASN")
		}
		info := Intelligence{ASN: uint32(asn), Provider: "UNKNOWN", Category: "UNKNOWN"}
		if len(parts) > 2 && strings.TrimSpace(parts[2]) != "" {
			info.Provider = strings.TrimSpace(parts[2])
		}
		if len(parts) > 3 && strings.TrimSpace(parts[3]) != "" {
			info.Category = strings.ToUpper(strings.TrimSpace(parts[3]))
		}
		if override := categoryOverrides[info.ASN]; override != "" {
			info.Category = override
		}
		db.records = append(db.records, NetworkRecord{Prefix: prefix.Masked(), Intelligence: info})
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.New("read local IP intelligence database failed")
	}
	return db, nil
}

func (db *LocalIntelligence) Lookup(ip netip.Addr) Intelligence {
	if db == nil {
		return Intelligence{Provider: "UNKNOWN", Category: "UNKNOWN"}
	}
	bestBits, result := -1, Intelligence{Provider: "UNKNOWN", Category: "UNKNOWN"}
	for _, record := range db.records {
		if record.Prefix.Contains(ip) && record.Prefix.Bits() > bestBits {
			bestBits, result = record.Prefix.Bits(), record.Intelligence
		}
	}
	return result
}
