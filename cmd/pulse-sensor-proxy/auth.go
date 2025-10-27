package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/rs/zerolog/log"
)

// peerCredentials holds extracted credentials from SO_PEERCRED
type peerCredentials struct {
	uid uint32
	pid uint32
	gid uint32
}

type idRange struct {
	start  uint64
	length uint64
}

func (r idRange) contains(v uint32) bool {
	value := uint64(v)
	return value >= r.start && value < r.start+r.length
}

// extractPeerCredentials extracts peer credentials via SO_PEERCRED
func extractPeerCredentials(conn net.Conn) (*peerCredentials, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, fmt.Errorf("not a unix connection")
	}

	file, err := unixConn.File()
	if err != nil {
		return nil, fmt.Errorf("failed to get file descriptor: %w", err)
	}
	defer file.Close()

	fd := int(file.Fd())

	cred, err := syscall.GetsockoptUcred(fd, syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	if err != nil {
		return nil, fmt.Errorf("failed to get peer credentials: %w", err)
	}

	log.Debug().
		Int32("pid", cred.Pid).
		Uint32("uid", cred.Uid).
		Uint32("gid", cred.Gid).
		Msg("Peer credentials")

	return &peerCredentials{
		uid: cred.Uid,
		pid: uint32(cred.Pid),
		gid: cred.Gid,
	}, nil
}

// initAuthRules builds in-memory allow lists for peer validation
func (p *Proxy) initAuthRules() error {
	p.allowedPeerUIDs = make(map[uint32]struct{})
	p.allowedPeerGIDs = make(map[uint32]struct{})

	// Always allow root and the proxy's own user
	p.allowedPeerUIDs[0] = struct{}{}
	p.allowedPeerUIDs[uint32(os.Getuid())] = struct{}{}
	p.allowedPeerGIDs[0] = struct{}{}
	p.allowedPeerGIDs[uint32(os.Getgid())] = struct{}{}

	if len(p.config.AllowedPeerUIDs) > 0 {
		for _, uid := range dedupeUint32(p.config.AllowedPeerUIDs) {
			p.allowedPeerUIDs[uid] = struct{}{}
		}
		log.Info().
			Int("explicit_uid_allow_count", len(p.config.AllowedPeerUIDs)).
			Msg("Loaded explicit peer UID allow-list entries")
	}

	if len(p.config.AllowedPeerGIDs) > 0 {
		for _, gid := range dedupeUint32(p.config.AllowedPeerGIDs) {
			p.allowedPeerGIDs[gid] = struct{}{}
		}
		log.Info().
			Int("explicit_gid_allow_count", len(p.config.AllowedPeerGIDs)).
			Msg("Loaded explicit peer GID allow-list entries")
	}

	if !p.config.AllowIDMappedRoot {
		log.Info().Msg("ID-mapped root authentication disabled")
		return nil
	}

	users := dedupeStrings(p.config.AllowedIDMapUsers)
	if len(users) == 0 {
		users = []string{"root"}
	}

	uidRanges, err := loadSubIDRanges("/etc/subuid", users)
	if err != nil {
		return fmt.Errorf("loading subordinate UID ranges: %w", err)
	}
	gidRanges, err := loadSubIDRanges("/etc/subgid", users)
	if err != nil {
		return fmt.Errorf("loading subordinate GID ranges: %w", err)
	}

	p.idMappedUIDRanges = uidRanges
	p.idMappedGIDRanges = gidRanges

	if len(uidRanges) == 0 || len(gidRanges) == 0 {
		log.Warn().
			Strs("users", users).
			Msg("allow_idmapped_root enabled but no subordinate ID ranges detected; LXC connections may fail")
	} else {
		log.Info().
			Int("uid_range_count", len(uidRanges)).
			Int("gid_range_count", len(gidRanges)).
			Strs("users", users).
			Msg("Loaded subordinate ID ranges for ID-mapped root authentication")
	}

	return nil
}

// authorizePeer verifies the peer credentials against configured allow lists
func (p *Proxy) authorizePeer(cred *peerCredentials) error {
	if _, ok := p.allowedPeerUIDs[cred.uid]; ok {
		return nil
	}

	if p.config.AllowIDMappedRoot && p.isIDMappedRoot(cred) {
		return nil
	}

	return fmt.Errorf("unauthorized: uid=%d gid=%d", cred.uid, cred.gid)
}

func (p *Proxy) isIDMappedRoot(cred *peerCredentials) bool {
	if len(p.idMappedUIDRanges) == 0 || len(p.idMappedGIDRanges) == 0 {
		return false
	}

	if !rangeContains(p.idMappedUIDRanges, cred.uid) {
		return false
	}

	if !rangeContains(p.idMappedGIDRanges, cred.gid) {
		return false
	}

	return true
}

func rangeContains(ranges []idRange, value uint32) bool {
	for _, r := range ranges {
		if r.contains(value) {
			return true
		}
	}
	return false
}

func dedupeUint32(values []uint32) []uint32 {
	seen := make(map[uint32]struct{})
	var result []uint32
	for _, val := range values {
		if _, ok := seen[val]; ok {
			continue
		}
		seen[val] = struct{}{}
		result = append(result, val)
	}
	return result
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{})
	var result []string
	for _, val := range values {
		trimmed := strings.TrimSpace(val)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

func loadSubIDRanges(path string, users []string) ([]idRange, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	userFilter := make(map[string]struct{}, len(users))
	for _, user := range users {
		userFilter[user] = struct{}{}
	}

	lines := strings.Split(string(data), "\n")
	var ranges []idRange

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.Split(line, ":")
		if len(parts) < 3 {
			continue
		}

		if len(userFilter) > 0 {
			if _, ok := userFilter[parts[0]]; !ok {
				continue
			}
		}

		start, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			log.Warn().
				Str("entry", line).
				Err(err).
				Msg("Skipping subordinate ID entry with invalid start value")
			continue
		}

		length, err := strconv.ParseUint(parts[2], 10, 64)
		if err != nil || length == 0 {
			log.Warn().
				Str("entry", line).
				Err(err).
				Msg("Skipping subordinate ID entry with invalid length")
			continue
		}

		ranges = append(ranges, idRange{start: start, length: length})
	}

	return ranges, nil
}
