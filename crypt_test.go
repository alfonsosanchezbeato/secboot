// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2019 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package secboot_test

import (
	"bytes"
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"time"

	snapd_testutil "github.com/snapcore/snapd/testutil"

	. "gopkg.in/check.v1"

	. "github.com/snapcore/secboot"
	"github.com/snapcore/secboot/internal/luks2"
	"github.com/snapcore/secboot/internal/luks2/luks2test"
	"github.com/snapcore/secboot/internal/luksview"
	"github.com/snapcore/secboot/internal/paths/pathstest"
	"github.com/snapcore/secboot/internal/testutil"
)

type cryptTestBase struct{}

func (ctb *cryptTestBase) newPrimaryKey() []byte {
	key := make([]byte, 32)
	rand.Read(key)
	return key
}

func (ctb *cryptTestBase) newRecoveryKey() RecoveryKey {
	var key RecoveryKey
	rand.Read(key[:])
	return key
}

type mockAuthRequestor struct {
	passphraseResponses []interface{}
	passphraseRequests  []struct {
		volumeName       string
		sourceDevicePath string
	}

	recoveryKeyResponses []interface{}
	recoveryKeyRequests  []struct {
		volumeName       string
		sourceDevicePath string
	}
}

func (r *mockAuthRequestor) RequestPassphrase(volumeName, sourceDevicePath string) (string, error) {
	r.passphraseRequests = append(r.passphraseRequests, struct {
		volumeName       string
		sourceDevicePath string
	}{
		volumeName:       volumeName,
		sourceDevicePath: sourceDevicePath,
	})

	if len(r.passphraseResponses) == 0 {
		return "", errors.New("empty response")
	}
	response := r.passphraseResponses[0]
	r.passphraseResponses = r.passphraseResponses[1:]

	switch rsp := response.(type) {
	case string:
		return rsp, nil
	case error:
		return "", rsp
	default:
		panic("invalid type")
	}
}

func (r *mockAuthRequestor) RequestRecoveryKey(volumeName, sourceDevicePath string) (RecoveryKey, error) {
	r.recoveryKeyRequests = append(r.recoveryKeyRequests, struct {
		volumeName       string
		sourceDevicePath string
	}{
		volumeName:       volumeName,
		sourceDevicePath: sourceDevicePath,
	})

	if len(r.recoveryKeyResponses) == 0 {
		return RecoveryKey{}, errors.New("empty response")
	}
	response := r.recoveryKeyResponses[0]
	r.recoveryKeyResponses = r.recoveryKeyResponses[1:]

	switch rsp := response.(type) {
	case RecoveryKey:
		return rsp, nil
	case error:
		return RecoveryKey{}, rsp
	default:
		panic("invalid type")
	}
}

// mockLUKS2Container represents a LUKS2 container and its associated state
type mockLUKS2Container struct {
	keyslots map[int][]byte
	tokens   map[int]luks2.Token
}

func (c *mockLUKS2Container) ReadHeader() (*luks2.HeaderInfo, error) {
	hdr := &luks2.HeaderInfo{
		Metadata: luks2.Metadata{
			Keyslots: make(map[int]*luks2.Keyslot),
			Tokens:   make(map[int]luks2.Token)}}

	for id := range c.keyslots {
		hdr.Metadata.Keyslots[id] = new(luks2.Keyslot)
	}
	for id, token := range c.tokens {
		hdr.Metadata.Tokens[id] = token
	}

	return hdr, nil
}

func (c *mockLUKS2Container) newLUKSView() (*luksview.View, error) {
	return luksview.NewViewFromCustomHeaderSource(c)
}

func (c *mockLUKS2Container) nextFreeTokenId() (id int) {
	var ids []int
	for i := range c.tokens {
		ids = append(ids, i)
	}
	sort.Ints(ids)
	for _, i := range ids {
		if i != id {
			break
		}
		id++
	}
	return id
}

func (c *mockLUKS2Container) nextFreeSlot() (slot int) {
	var slots []int
	for i := range c.keyslots {
		slots = append(slots, i)
	}
	sort.Ints(slots)
	for _, i := range slots {
		if i != slot {
			break
		}
		slot++
	}
	return slot
}

// mockLUKS2 mocks a device's global LUKS2 state. It provides mock
// implementations of the various LUKS2 operations.
type mockLUKS2 struct {
	operations []string                       // A log of LUKS2 operations recorded during a test
	devices    map[string]*mockLUKS2Container // A map of device paths to mocked containers
	activated  map[string]string              // A map of volume names to device paths for activated containers.
}

func (l *mockLUKS2) activate(volumeName, sourceDevicePath string, key []byte) error {
	l.operations = append(l.operations, "Activate("+volumeName+","+sourceDevicePath+")")

	if _, exists := l.activated[volumeName]; exists {
		return errors.New("systemd-cryptsetup failed with: exit status 1")
	}

	dev, ok := l.devices[sourceDevicePath]
	if !ok {
		return errors.New("systemd-cryptsetup failed with: exit status 1")
	}

	for _, k := range dev.keyslots {
		if bytes.Equal(k, key) {
			l.activated[volumeName] = sourceDevicePath
			return nil
		}
	}

	return errors.New("systemd-cryptsetup failed with: exit status 1")
}

func (l *mockLUKS2) addKey(devicePath string, existingKey, key []byte, options *luks2.AddKeyOptions) error {
	l.operations = append(l.operations, fmt.Sprint("AddKey(", devicePath, ",", options, ")"))

	if options == nil {
		options = &luks2.AddKeyOptions{Slot: luks2.AnySlot}
	}

	dev, ok := l.devices[devicePath]
	if !ok {
		return errors.New("no container")
	}

	var slot int
	switch {
	case options.Slot == luks2.AnySlot:
		slot = dev.nextFreeSlot()
	case options.Slot < 0:
		return errors.New("invalid slot")
	default:
		if _, exists := dev.keyslots[options.Slot]; exists {
			return errors.New("slot already in use")
		}
		slot = options.Slot
	}

	found := false
	for _, k := range dev.keyslots {
		if bytes.Equal(k, existingKey) {
			found = true
			break
		}
	}
	if !found {
		return errors.New("invalid key")
	}

	dev.keyslots[slot] = key
	return nil
}

func (l *mockLUKS2) deactivate(volumeName string) error {
	l.operations = append(l.operations, "Deactivate("+volumeName+")")

	if _, exists := l.activated[volumeName]; !exists {
		return errors.New("systemd-cryptsetup failed with: exit status 1")
	}

	delete(l.activated, volumeName)
	return nil
}

func (l *mockLUKS2) format(devicePath, label string, key []byte, options *luks2.FormatOptions) error {
	l.operations = append(l.operations, fmt.Sprint("Format(", devicePath, ",", label, ",", options, ")"))

	l.devices[devicePath] = &mockLUKS2Container{
		keyslots: map[int][]byte{0: key},
		tokens:   make(map[int]luks2.Token)}
	return nil
}

func (l *mockLUKS2) importToken(devicePath string, token luks2.Token, options *luks2.ImportTokenOptions) error {
	l.operations = append(l.operations, fmt.Sprint("ImportToken(", devicePath, ",", options, ")"))

	dev, ok := l.devices[devicePath]
	if !ok {
		return errors.New("no container")
	}

	if options == nil {
		options = &luks2.ImportTokenOptions{Id: luks2.AnyId}
	}

	var id int
	switch {
	case options.Id == luks2.AnyId:
		id = dev.nextFreeTokenId()
	case options.Id < 0:
		return errors.New("invalid id")
	default:
		if _, exists := dev.tokens[options.Id]; exists && !options.Replace {
			return errors.New("id already in use")
		}
		id = options.Id
	}

	dev.tokens[id] = token
	return nil
}

func (l *mockLUKS2) killSlot(devicePath string, slot int, key []byte) error {
	l.operations = append(l.operations, fmt.Sprint("KillSlot(", devicePath, ",", slot, ")"))

	if slot < 0 {
		return errors.New("invalid slot")
	}

	dev, ok := l.devices[devicePath]
	if !ok {
		return errors.New("no container")
	}

	if _, exists := dev.keyslots[slot]; !exists {
		return errors.New("no slot")
	}

	if len(dev.keyslots) == 1 {
		if !bytes.Equal(key, dev.keyslots[slot]) {
			return errors.New("invalid key")
		}
	} else {
		found := false
		for i, k := range dev.keyslots {
			if i == slot {
				continue
			}
			if bytes.Equal(key, k) {
				found = true
				break
			}
		}
		if !found {
			return errors.New("invalid key")
		}
	}

	delete(dev.keyslots, slot)
	return nil
}

func (l *mockLUKS2) removeToken(devicePath string, id int) error {
	l.operations = append(l.operations, "RemoveToken("+devicePath+","+strconv.Itoa(id)+")")

	dev, ok := l.devices[devicePath]
	if !ok {
		return errors.New("no container")
	}

	if _, exists := dev.tokens[id]; !exists {
		return errors.New("no token")
	}

	delete(dev.tokens, id)
	return nil
}

func (l *mockLUKS2) setSlotPriority(devicePath string, slot int, priority luks2.SlotPriority) error {
	l.operations = append(l.operations, fmt.Sprint("SetSlotPriority(", devicePath, ",", slot, ",", priority, ")"))

	if _, ok := l.devices[devicePath]; !ok {
		return errors.New("no container")
	}
	return nil
}

func (l *mockLUKS2) newLUKSView(devicePath string, lockMode luks2.LockMode) (*luksview.View, error) {
	l.operations = append(l.operations, fmt.Sprint("newLUKSView(", devicePath, ",", lockMode, ")"))

	dev, ok := l.devices[devicePath]
	if !ok {
		return nil, errors.New("no container")
	}
	return dev.newLUKSView()
}

type cryptSuite struct {
	cryptTestBase
	keyDataTestBase
	testutil.KeyringTestBase

	luks2 *mockLUKS2
}

var _ = Suite(&cryptSuite{})

func (s *cryptSuite) SetUpSuite(c *C) {
	s.keyDataTestBase.SetUpSuite(c)
	s.KeyringTestBase.SetUpSuite(c)
}

func (s *cryptSuite) SetUpTest(c *C) {
	s.keyDataTestBase.SetUpTest(c)
	s.KeyringTestBase.SetUpTest(c)

	s.handler.passphraseSupport = true

	s.AddCleanup(pathstest.MockRunDir(c.MkDir()))

	s.luks2 = &mockLUKS2{
		devices:   make(map[string]*mockLUKS2Container),
		activated: make(map[string]string)}

	restore := MockLUKS2Activate(s.luks2.activate)
	s.AddCleanup(restore)
	restore = MockLUKS2AddKey(s.luks2.addKey)
	s.AddCleanup(restore)
	restore = MockLUKS2Deactivate(s.luks2.deactivate)
	s.AddCleanup(restore)
	restore = MockLUKS2Format(s.luks2.format)
	s.AddCleanup(restore)
	restore = MockLUKS2ImportToken(s.luks2.importToken)
	s.AddCleanup(restore)
	restore = MockLUKS2KillSlot(s.luks2.killSlot)
	s.AddCleanup(restore)
	restore = MockLUKS2RemoveToken(s.luks2.removeToken)
	s.AddCleanup(restore)
	restore = MockLUKS2SetSlotPriority(s.luks2.setSlotPriority)
	s.AddCleanup(restore)
	restore = MockNewLUKSView(s.luks2.newLUKSView)
	s.AddCleanup(restore)
}

func (s *cryptSuite) addMockKeyslot(path string, key []byte) {
	dev, ok := s.luks2.devices[path]
	if !ok {
		dev = &mockLUKS2Container{keyslots: make(map[int][]byte)}
		s.luks2.devices[path] = dev
	}
	dev.keyslots[dev.nextFreeSlot()] = key
}

func (s *cryptSuite) checkRecoveryKeyInKeyring(c *C, prefix, path string, expected RecoveryKey) {
	// The following test will fail if the user keyring isn't reachable from the session keyring. If the test have succeeded
	// so far, mark the current test as expected to fail.
	if !s.ProcessPossessesUserKeyringKeys && !c.Failed() {
		c.ExpectFailure("Cannot possess user keys because the user keyring isn't reachable from the session keyring")
	}

	key, err := GetDiskUnlockKeyFromKernel(prefix, path, false)
	c.Check(err, IsNil)
	c.Check(key, DeepEquals, DiskUnlockKey(expected[:]))
}

func (s *cryptSuite) checkKeyDataKeysInKeyring(c *C, prefix, path string, expectedKey DiskUnlockKey, expectedAuxKey AuxiliaryKey) {
	// The following test will fail if the user keyring isn't reachable from the session keyring. If the test have succeeded
	// so far, mark the current test as expected to fail.
	if !s.ProcessPossessesUserKeyringKeys && !c.Failed() {
		c.ExpectFailure("Cannot possess user keys because the user keyring isn't reachable from the session keyring")
	}

	key, err := GetDiskUnlockKeyFromKernel(prefix, path, false)
	c.Check(err, IsNil)
	c.Check(key, DeepEquals, expectedKey)

	auxKey, err := GetAuxiliaryKeyFromKernel(prefix, path, false)
	c.Check(err, IsNil)
	c.Check(auxKey, DeepEquals, expectedAuxKey)
}

func (s *cryptSuite) newMultipleNamedKeyData(c *C, names ...string) (keyData []*KeyData, keys []DiskUnlockKey, auxKeys []AuxiliaryKey) {
	for _, name := range names {
		key, auxKey := s.newKeyDataKeys(c, 32, 32)
		protected := s.mockProtectKeys(c, key, auxKey, crypto.SHA256)

		kd, err := NewKeyData(protected)
		c.Assert(err, IsNil)

		w := makeMockKeyDataWriter()
		c.Check(kd.WriteAtomic(w), IsNil)

		r := &mockKeyDataReader{name, w.Reader()}
		kd, err = ReadKeyData(r)
		c.Assert(err, IsNil)

		keyData = append(keyData, kd)
		keys = append(keys, key)
		auxKeys = append(auxKeys, auxKey)
	}

	return keyData, keys, auxKeys
}

func (s *cryptSuite) newNamedKeyData(c *C, name string) (*KeyData, DiskUnlockKey, AuxiliaryKey) {
	keyData, keys, auxKeys := s.newMultipleNamedKeyData(c, name)
	return keyData[0], keys[0], auxKeys[0]
}

type testActivateVolumeWithRecoveryKeyData struct {
	recoveryKey      RecoveryKey
	volumeName       string
	sourceDevicePath string
	tries            int
	keyringPrefix    string
	authResponses    []interface{}
	activateTries    int
}

func (s *cryptSuite) testActivateVolumeWithRecoveryKey(c *C, data *testActivateVolumeWithRecoveryKeyData) {
	s.addMockKeyslot(data.sourceDevicePath, data.recoveryKey[:])

	authRequestor := &mockAuthRequestor{recoveryKeyResponses: data.authResponses}
	options := ActivateVolumeOptions{RecoveryKeyTries: data.tries, KeyringPrefix: data.keyringPrefix}

	c.Assert(ActivateVolumeWithRecoveryKey(data.volumeName, data.sourceDevicePath, authRequestor, &options), IsNil)

	c.Check(authRequestor.recoveryKeyRequests, HasLen, len(data.authResponses))
	for _, rsp := range authRequestor.recoveryKeyRequests {
		c.Check(rsp.volumeName, Equals, data.volumeName)
		c.Check(rsp.sourceDevicePath, Equals, data.sourceDevicePath)
	}

	c.Assert(s.luks2.operations, HasLen, data.activateTries)
	for _, op := range s.luks2.operations {
		c.Check(op, Equals, "Activate("+data.volumeName+","+data.sourceDevicePath+")")
	}

	// This should be done last because it may fail in some circumstances.
	s.checkRecoveryKeyInKeyring(c, data.keyringPrefix, data.sourceDevicePath, data.recoveryKey)
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKey1(c *C) {
	// Test with the correct recovery key.
	recoveryKey := s.newRecoveryKey()
	s.testActivateVolumeWithRecoveryKey(c, &testActivateVolumeWithRecoveryKeyData{
		recoveryKey:      recoveryKey,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		tries:            1,
		authResponses:    []interface{}{recoveryKey},
		activateTries:    1,
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKey2(c *C) {
	// Test that activation succeeds when the correct recovery key is provided on the second attempt.
	recoveryKey := s.newRecoveryKey()
	s.testActivateVolumeWithRecoveryKey(c, &testActivateVolumeWithRecoveryKeyData{
		recoveryKey:      recoveryKey,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		tries:            2,
		authResponses:    []interface{}{RecoveryKey{}, recoveryKey},
		activateTries:    2,
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKey3(c *C) {
	// Test that activation succeeds when the correct recovery key is provided on the second attempt, and the first
	// attempt is an error.
	recoveryKey := s.newRecoveryKey()
	s.testActivateVolumeWithRecoveryKey(c, &testActivateVolumeWithRecoveryKeyData{
		recoveryKey:      recoveryKey,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		tries:            2,
		authResponses:    []interface{}{errors.New(""), recoveryKey},
		activateTries:    1,
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKey4(c *C) {
	// Test with a different volume name / device path.
	recoveryKey := s.newRecoveryKey()
	s.testActivateVolumeWithRecoveryKey(c, &testActivateVolumeWithRecoveryKeyData{
		recoveryKey:      recoveryKey,
		volumeName:       "foo",
		sourceDevicePath: "/dev/vdb2",
		tries:            1,
		authResponses:    []interface{}{recoveryKey},
		activateTries:    1,
	})
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKey5(c *C) {
	// Test with a different keyring prefix
	recoveryKey := s.newRecoveryKey()
	s.testActivateVolumeWithRecoveryKey(c, &testActivateVolumeWithRecoveryKeyData{
		recoveryKey:      recoveryKey,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		tries:            1,
		keyringPrefix:    "test",
		authResponses:    []interface{}{recoveryKey},
		activateTries:    1,
	})
}

type testParseRecoveryKeyData struct {
	formatted string
	expected  []byte
}

func (s *cryptSuite) testParseRecoveryKey(c *C, data *testParseRecoveryKeyData) {
	k, err := ParseRecoveryKey(data.formatted)
	c.Check(err, IsNil)
	c.Check(k[:], DeepEquals, data.expected)
}

func (s *cryptSuite) TestParseRecoveryKey1(c *C) {
	s.testParseRecoveryKey(c, &testParseRecoveryKeyData{
		formatted: "00000-00000-00000-00000-00000-00000-00000-00000",
		expected:  testutil.DecodeHexString(c, "00000000000000000000000000000000"),
	})
}

func (s *cryptSuite) TestParseRecoveryKey2(c *C) {
	s.testParseRecoveryKey(c, &testParseRecoveryKeyData{
		formatted: "61665-00531-54469-09783-47273-19035-40077-28287",
		expected:  testutil.DecodeHexString(c, "e1f01302c5d43726a9b85b4a8d9c7f6e"),
	})
}

func (s *cryptSuite) TestParseRecoveryKey3(c *C) {
	s.testParseRecoveryKey(c, &testParseRecoveryKeyData{
		formatted: "6166500531544690978347273190354007728287",
		expected:  testutil.DecodeHexString(c, "e1f01302c5d43726a9b85b4a8d9c7f6e"),
	})
}

type testParseRecoveryKeyErrorHandlingData struct {
	formatted      string
	errChecker     Checker
	errCheckerArgs []interface{}
}

func (s *cryptSuite) testParseRecoveryKeyErrorHandling(c *C, data *testParseRecoveryKeyErrorHandlingData) {
	_, err := ParseRecoveryKey(data.formatted)
	c.Check(err, data.errChecker, data.errCheckerArgs...)
}

func (s *cryptSuite) TestParseRecoveryKeyErrorHandling1(c *C) {
	s.testParseRecoveryKeyErrorHandling(c, &testParseRecoveryKeyErrorHandlingData{
		formatted:      "00000-1234",
		errChecker:     ErrorMatches,
		errCheckerArgs: []interface{}{"incorrectly formatted: insufficient characters"},
	})
}

func (s *cryptSuite) TestParseRecoveryKeyErrorHandling2(c *C) {
	s.testParseRecoveryKeyErrorHandling(c, &testParseRecoveryKeyErrorHandlingData{
		formatted:      "00000-123bc",
		errChecker:     ErrorMatches,
		errCheckerArgs: []interface{}{"incorrectly formatted: strconv.ParseUint: parsing \"123bc\": invalid syntax"},
	})
}

func (s *cryptSuite) TestParseRecoveryKeyErrorHandling3(c *C) {
	s.testParseRecoveryKeyErrorHandling(c, &testParseRecoveryKeyErrorHandlingData{
		formatted:      "00000-00000-00000-00000-00000-00000-00000-00000-00000",
		errChecker:     ErrorMatches,
		errCheckerArgs: []interface{}{"incorrectly formatted: too many characters"},
	})
}

func (s *cryptSuite) TestParseRecoveryKeyErrorHandling4(c *C) {
	s.testParseRecoveryKeyErrorHandling(c, &testParseRecoveryKeyErrorHandlingData{
		formatted:      "-00000-00000-00000-00000-00000-00000-00000-00000",
		errChecker:     ErrorMatches,
		errCheckerArgs: []interface{}{"incorrectly formatted: strconv.ParseUint: parsing \"-0000\": invalid syntax"},
	})
}

func (s *cryptSuite) TestParseRecoveryKeyErrorHandling5(c *C) {
	s.testParseRecoveryKeyErrorHandling(c, &testParseRecoveryKeyErrorHandlingData{
		formatted:      "00000-00000-00000-00000-00000-00000-00000-00000-",
		errChecker:     ErrorMatches,
		errCheckerArgs: []interface{}{"incorrectly formatted: too many characters"},
	})
}

type testRecoveryKeyStringifyData struct {
	key      []byte
	expected string
}

func (s *cryptSuite) testRecoveryKeyStringify(c *C, data *testRecoveryKeyStringifyData) {
	var key RecoveryKey
	copy(key[:], data.key)
	c.Check(key.String(), Equals, data.expected)
}

func (s *cryptSuite) TestRecoveryKeyStringify1(c *C) {
	s.testRecoveryKeyStringify(c, &testRecoveryKeyStringifyData{
		expected: "00000-00000-00000-00000-00000-00000-00000-00000",
	})
}

func (s *cryptSuite) TestRecoveryKeyStringify2(c *C) {
	s.testRecoveryKeyStringify(c, &testRecoveryKeyStringifyData{
		key:      testutil.DecodeHexString(c, "e1f01302c5d43726a9b85b4a8d9c7f6e"),
		expected: "61665-00531-54469-09783-47273-19035-40077-28287",
	})
}

type testActivateVolumeWithRecoveryKeyErrorHandlingData struct {
	tries         int
	authRequestor *mockAuthRequestor
	activateTries int
}

func (s *cryptSuite) testActivateVolumeWithRecoveryKeyErrorHandling(c *C, data *testActivateVolumeWithRecoveryKeyErrorHandlingData) error {
	recoveryKey := s.newRecoveryKey()
	s.addMockKeyslot("/dev/sda1", recoveryKey[:])

	var authRequestor AuthRequestor
	var numResponses int
	if data.authRequestor != nil {
		authRequestor = data.authRequestor
		numResponses = len(data.authRequestor.recoveryKeyResponses)
	}

	options := ActivateVolumeOptions{RecoveryKeyTries: data.tries}
	err := ActivateVolumeWithRecoveryKey("data", "/dev/sda1", authRequestor, &options)

	if data.authRequestor != nil {
		c.Check(data.authRequestor.recoveryKeyRequests, HasLen, numResponses)
		for _, rsp := range data.authRequestor.recoveryKeyRequests {
			c.Check(rsp.volumeName, Equals, "data")
			c.Check(rsp.sourceDevicePath, Equals, "/dev/sda1")
		}
	}

	c.Assert(s.luks2.operations, HasLen, data.activateTries)
	for _, op := range s.luks2.operations {
		c.Check(op, Equals, "Activate(data,/dev/sda1)")
	}

	return err
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyErrorHandling1(c *C) {
	// Test with an invalid RecoveryKeyTries value.
	c.Check(s.testActivateVolumeWithRecoveryKeyErrorHandling(c, &testActivateVolumeWithRecoveryKeyErrorHandlingData{
		tries:         -1,
		authRequestor: &mockAuthRequestor{},
	}), ErrorMatches, "invalid RecoveryKeyTries")
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyErrorHandling2(c *C) {
	// Test with Tries set to zero.
	c.Check(s.testActivateVolumeWithRecoveryKeyErrorHandling(c, &testActivateVolumeWithRecoveryKeyErrorHandlingData{
		tries:         0,
		authRequestor: &mockAuthRequestor{},
	}), ErrorMatches, "no recovery key tries permitted")
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyErrorHandling3(c *C) {
	// Test with no auth requestor
	c.Check(s.testActivateVolumeWithRecoveryKeyErrorHandling(c, &testActivateVolumeWithRecoveryKeyErrorHandlingData{
		tries: 1,
	}), ErrorMatches, "nil authRequestor")
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyErrorHandling4(c *C) {
	// Test with the auth requestor returning an error.
	c.Check(s.testActivateVolumeWithRecoveryKeyErrorHandling(c, &testActivateVolumeWithRecoveryKeyErrorHandlingData{
		tries:         1,
		authRequestor: &mockAuthRequestor{recoveryKeyResponses: []interface{}{errors.New("some error")}},
	}), ErrorMatches, "cannot obtain recovery key: some error")
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyErrorHandling5(c *C) {
	// Test with the wrong recovery key.
	c.Check(s.testActivateVolumeWithRecoveryKeyErrorHandling(c, &testActivateVolumeWithRecoveryKeyErrorHandlingData{
		tries:         1,
		authRequestor: &mockAuthRequestor{recoveryKeyResponses: []interface{}{RecoveryKey{}}},
		activateTries: 1,
	}), ErrorMatches, "cannot activate volume: systemd-cryptsetup failed with: exit status 1")
}

func (s *cryptSuite) TestActivateVolumeWithRecoveryKeyErrorHandling6(c *C) {
	// Test that the last error is returned when there are consecutive failures for different reasons.
	c.Check(s.testActivateVolumeWithRecoveryKeyErrorHandling(c, &testActivateVolumeWithRecoveryKeyErrorHandlingData{
		tries:         2,
		authRequestor: &mockAuthRequestor{recoveryKeyResponses: []interface{}{errors.New("some error"), errors.New("another error")}},
		activateTries: 0,
	}), ErrorMatches, "cannot obtain recovery key: another error")
}

type testActivateVolumeWithKeyDataData struct {
	authorizedModels []SnapModel
	passphrase       string
	volumeName       string
	sourceDevicePath string
	passphraseTries  int
	keyringPrefix    string
	authResponses    []interface{}
	model            SnapModel
}

func (s *cryptSuite) testActivateVolumeWithKeyData(c *C, data *testActivateVolumeWithKeyDataData) {
	keyData, key, auxKey := s.newNamedKeyData(c, "")
	s.addMockKeyslot(data.sourceDevicePath, key)

	c.Check(keyData.SetAuthorizedSnapModels(auxKey, data.authorizedModels...), IsNil)

	authRequestor := &mockAuthRequestor{passphraseResponses: data.authResponses}

	var kdf mockKDF
	if data.passphrase != "" {
		c.Check(keyData.SetPassphrase(data.passphrase, nil, &kdf), IsNil)
	}

	options := &ActivateVolumeOptions{
		PassphraseTries: data.passphraseTries,
		KeyringPrefix:   data.keyringPrefix,
		Model:           data.model}
	err := ActivateVolumeWithKeyData(data.volumeName, data.sourceDevicePath, keyData, authRequestor, &kdf, options)
	c.Assert(err, IsNil)

	c.Check(s.luks2.operations, DeepEquals, []string{"Activate(" + data.volumeName + "," + data.sourceDevicePath + ")"})

	c.Check(authRequestor.passphraseRequests, HasLen, len(data.authResponses))
	for _, rsp := range authRequestor.passphraseRequests {
		c.Check(rsp.volumeName, Equals, data.volumeName)
		c.Check(rsp.sourceDevicePath, Equals, data.sourceDevicePath)
	}

	// This should be done last because it may fail in some circumstances.
	s.checkKeyDataKeysInKeyring(c, data.keyringPrefix, data.sourceDevicePath, key, auxKey)
}

func (s *cryptSuite) TestActivateVolumeWithKeyData1(c *C) {
	models := []SnapModel{
		testutil.MakeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}

	s.testActivateVolumeWithKeyData(c, &testActivateVolumeWithKeyDataData{
		authorizedModels: models,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		model:            models[0]})
}

func (s *cryptSuite) TestActivateVolumeWithKeyData2(c *C) {
	// Test with different volumeName / sourceDevicePath
	models := []SnapModel{
		testutil.MakeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}

	s.testActivateVolumeWithKeyData(c, &testActivateVolumeWithKeyDataData{
		authorizedModels: models,
		volumeName:       "foo",
		sourceDevicePath: "/dev/vda2",
		model:            models[0]})
}

func (s *cryptSuite) TestActivateVolumeWithKeyData3(c *C) {
	// Test with different authorized models
	models := []SnapModel{
		testutil.MakeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij"),
		testutil.MakeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "other-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}

	s.testActivateVolumeWithKeyData(c, &testActivateVolumeWithKeyDataData{
		authorizedModels: models,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		model:            models[0]})
}

func (s *cryptSuite) TestActivateVolumeWithKeyData4(c *C) {
	// Test that skipping the snap model check works
	s.testActivateVolumeWithKeyData(c, &testActivateVolumeWithKeyDataData{
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		model:            SkipSnapModelCheck})
}

func (s *cryptSuite) TestActivateVolumeWithKeyData5(c *C) {
	// Test with passphrase
	models := []SnapModel{
		testutil.MakeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}

	s.testActivateVolumeWithKeyData(c, &testActivateVolumeWithKeyDataData{
		passphrase:       "1234",
		authorizedModels: models,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		passphraseTries:  1,
		authResponses:    []interface{}{"1234"},
		model:            models[0]})
}

func (s *cryptSuite) TestActivateVolumeWithKeyData6(c *C) {
	// Test with passphrase using multiple tries
	models := []SnapModel{
		testutil.MakeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}

	s.testActivateVolumeWithKeyData(c, &testActivateVolumeWithKeyDataData{
		passphrase:       "1234",
		authorizedModels: models,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		passphraseTries:  3,
		authResponses:    []interface{}{"incorrect", "1234"},
		model:            models[0]})
}

type testActivateVolumeWithKeyDataErrorHandlingData struct {
	primaryKey  DiskUnlockKey
	recoveryKey RecoveryKey

	authRequestor *mockAuthRequestor

	passphraseTries  int
	recoveryKeyTries int
	keyringPrefix    string
	kdf              KDF

	keyData *KeyData

	model SnapModel

	activateTries int
}

func (s *cryptSuite) testActivateVolumeWithKeyDataErrorHandling(c *C, data *testActivateVolumeWithKeyDataErrorHandlingData) error {
	s.addMockKeyslot("/dev/sda1", data.primaryKey)
	s.addMockKeyslot("/dev/sda1", data.recoveryKey[:])

	var authRequestor AuthRequestor
	var numPassphraseResponses int
	var numRecoveryResponses int
	if data.authRequestor != nil {
		authRequestor = data.authRequestor
		numPassphraseResponses = len(data.authRequestor.passphraseResponses)
		numRecoveryResponses = len(data.authRequestor.recoveryKeyResponses)
	}

	options := &ActivateVolumeOptions{
		PassphraseTries:  data.passphraseTries,
		RecoveryKeyTries: data.recoveryKeyTries,
		KeyringPrefix:    data.keyringPrefix,
		Model:            data.model}
	err := ActivateVolumeWithKeyData("data", "/dev/sda1", data.keyData, authRequestor, data.kdf, options)

	if data.authRequestor != nil {
		c.Check(data.authRequestor.passphraseRequests, HasLen, numPassphraseResponses)
		for _, rsp := range data.authRequestor.passphraseRequests {
			c.Check(rsp.volumeName, Equals, "data")
			c.Check(rsp.sourceDevicePath, Equals, "/dev/sda1")
		}

		c.Check(data.authRequestor.recoveryKeyRequests, HasLen, numRecoveryResponses)
		for _, rsp := range data.authRequestor.recoveryKeyRequests {
			c.Check(rsp.volumeName, Equals, "data")
			c.Check(rsp.sourceDevicePath, Equals, "/dev/sda1")
		}
	}

	c.Assert(s.luks2.operations, HasLen, data.activateTries)
	for _, op := range s.luks2.operations {
		c.Check(op, Equals, "Activate(data,/dev/sda1)")
	}

	if err == ErrRecoveryKeyUsed {
		// This should be done last because it may fail in some circumstances.
		s.checkRecoveryKeyInKeyring(c, data.keyringPrefix, "/dev/sda1", data.recoveryKey)
	}

	return err
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling1(c *C) {
	// Test with an invalid value for RecoveryKeyTries.
	keyData, _, _ := s.newNamedKeyData(c, "")

	c.Check(s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		recoveryKeyTries: -1,
		keyData:          keyData,
	}), ErrorMatches, "invalid RecoveryKeyTries")
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling2(c *C) {
	// Test that recovery fallback works with the platform is unavailable
	keyData, key, _ := s.newNamedKeyData(c, "")
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	c.Check(s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		primaryKey:       key,
		recoveryKey:      recoveryKey,
		authRequestor:    &mockAuthRequestor{recoveryKeyResponses: []interface{}{recoveryKey}},
		recoveryKeyTries: 1,
		keyData:          keyData,
		model:            SkipSnapModelCheck,
		activateTries:    1,
	}), Equals, ErrRecoveryKeyUsed)
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling3(c *C) {
	// Test that recovery fallback works when the platform device isn't properly initialized
	keyData, key, _ := s.newNamedKeyData(c, "")
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUninitialized

	c.Check(s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		primaryKey:       key,
		recoveryKey:      recoveryKey,
		authRequestor:    &mockAuthRequestor{recoveryKeyResponses: []interface{}{recoveryKey}},
		recoveryKeyTries: 1,
		keyData:          keyData,
		model:            SkipSnapModelCheck,
		activateTries:    1,
	}), Equals, ErrRecoveryKeyUsed)
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling4(c *C) {
	// Test that recovery fallback works when the recovered key is incorrect
	keyData, _, _ := s.newNamedKeyData(c, "")
	recoveryKey := s.newRecoveryKey()

	c.Check(s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		recoveryKey:      recoveryKey,
		authRequestor:    &mockAuthRequestor{recoveryKeyResponses: []interface{}{recoveryKey}},
		recoveryKeyTries: 1,
		keyData:          keyData,
		model:            SkipSnapModelCheck,
		activateTries:    2,
	}), Equals, ErrRecoveryKeyUsed)
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling5(c *C) {
	// Test that activation fails if RecoveryKeyTries is zero.
	keyData, key, _ := s.newNamedKeyData(c, "foo")
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	c.Check(s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		primaryKey:       key,
		recoveryKey:      recoveryKey,
		recoveryKeyTries: 0,
		keyData:          keyData,
		model:            SkipSnapModelCheck,
		activateTries:    0,
	}), ErrorMatches,
		"cannot activate with platform protected keys:\n"+
			"- foo: cannot recover key: the platform's secure device is unavailable: the "+
			"platform device is unavailable\n"+
			"and activation with recovery key failed: no recovery key tries permitted")
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling6(c *C) {
	// Test that activation fails if the supplied recovery key is incorrect
	keyData, key, _ := s.newNamedKeyData(c, "bar")
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	c.Check(s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		primaryKey:       key,
		recoveryKey:      recoveryKey,
		authRequestor:    &mockAuthRequestor{recoveryKeyResponses: []interface{}{RecoveryKey{}}},
		recoveryKeyTries: 1,
		keyData:          keyData,
		model:            SkipSnapModelCheck,
		activateTries:    1,
	}), ErrorMatches,
		"cannot activate with platform protected keys:\n"+
			"- bar: cannot recover key: the platform's secure device is unavailable: the "+
			"platform device is unavailable\n"+
			"and activation with recovery key failed: cannot activate volume: "+
			"systemd-cryptsetup failed with: exit status 1")
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling7(c *C) {
	// Test that recovery fallback works if the correct key is eventually supplied
	keyData, key, _ := s.newNamedKeyData(c, "")
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	c.Check(s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		primaryKey:       key,
		recoveryKey:      recoveryKey,
		authRequestor:    &mockAuthRequestor{recoveryKeyResponses: []interface{}{RecoveryKey{}, recoveryKey}},
		recoveryKeyTries: 2,
		keyData:          keyData,
		model:            SkipSnapModelCheck,
		activateTries:    2,
	}), Equals, ErrRecoveryKeyUsed)
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling8(c *C) {
	// Test that recovery fallback works if the correct key is eventually supplied
	keyData, key, _ := s.newNamedKeyData(c, "")
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	c.Check(s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		primaryKey:       key,
		recoveryKey:      recoveryKey,
		authRequestor:    &mockAuthRequestor{recoveryKeyResponses: []interface{}{errors.New("some error"), recoveryKey}},
		recoveryKeyTries: 2,
		keyData:          keyData,
		model:            SkipSnapModelCheck,
		activateTries:    1,
	}), Equals, ErrRecoveryKeyUsed)
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling9(c *C) {
	// Test that we get an error if no AuthRequestor is supplied but recovery key
	// tries are permitted.
	keyData, _, _ := s.newNamedKeyData(c, "")

	c.Check(s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		recoveryKeyTries: 1,
		keyData:          keyData,
		model:            SkipSnapModelCheck,
	}), ErrorMatches, "nil authRequestor")
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling10(c *C) {
	// Test that recovery key fallback works if the wrong passphrase is supplied.
	keyData, key, _ := s.newNamedKeyData(c, "foo")
	recoveryKey := s.newRecoveryKey()

	var kdf mockKDF
	c.Check(keyData.SetPassphrase("1234", nil, &kdf), IsNil)

	c.Check(s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		primaryKey:  key,
		recoveryKey: recoveryKey,
		authRequestor: &mockAuthRequestor{
			passphraseResponses:  []interface{}{"incorrect", "invalid"},
			recoveryKeyResponses: []interface{}{recoveryKey}},
		passphraseTries:  2,
		recoveryKeyTries: 2,
		kdf:              &kdf,
		keyData:          keyData,
		model:            SkipSnapModelCheck,
		activateTries:    1,
	}), Equals, ErrRecoveryKeyUsed)
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling11(c *C) {
	// Test that we get an error if no AuthRequestor is supplied but passphrase
	// tries are permitted.
	keyData, _, _ := s.newNamedKeyData(c, "")

	c.Check(s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		passphraseTries: 1,
		keyData:         keyData,
		model:           SkipSnapModelCheck,
	}), ErrorMatches, "nil authRequestor")
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling12(c *C) {
	// Test that we get an error if no KDF is supplied but passphrase tries
	// are permitted.
	keyData, _, _ := s.newNamedKeyData(c, "")

	c.Check(s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		passphraseTries: 1,
		keyData:         keyData,
		model:           SkipSnapModelCheck,
		authRequestor:   &mockAuthRequestor{},
	}), ErrorMatches, "nil kdf")
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling13(c *C) {
	// Test that activation fails if the supplied passphrase and recovery key are incorrect
	keyData, key, _ := s.newNamedKeyData(c, "bar")
	recoveryKey := s.newRecoveryKey()

	var kdf mockKDF
	c.Check(keyData.SetPassphrase("1234", nil, &kdf), IsNil)

	c.Check(s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		primaryKey:  key,
		recoveryKey: recoveryKey,
		authRequestor: &mockAuthRequestor{
			passphraseResponses:  []interface{}{""},
			recoveryKeyResponses: []interface{}{RecoveryKey{}}},
		passphraseTries:  1,
		recoveryKeyTries: 1,
		kdf:              &kdf,
		keyData:          keyData,
		model:            SkipSnapModelCheck,
		activateTries:    1,
	}), ErrorMatches, "cannot activate with platform protected keys:\n"+
		"- bar: cannot recover key: the supplied passphrase is incorrect\n"+
		"and activation with recovery key failed: cannot activate volume: "+
		"systemd-cryptsetup failed with: exit status 1")
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling14(c *C) {
	// Test with an invalid value for SnapModel
	keyData, _, _ := s.newNamedKeyData(c, "")

	c.Check(s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		keyData: keyData,
	}), ErrorMatches, "nil Model")
}

func (s *cryptSuite) TestActivateVolumeWithKeyDataErrorHandling15(c *C) {
	// Test that activation fails if the supplied model is not authorized
	keyData, key, _ := s.newNamedKeyData(c, "foo")
	recoveryKey := s.newRecoveryKey()

	c.Check(s.testActivateVolumeWithKeyDataErrorHandling(c, &testActivateVolumeWithKeyDataErrorHandlingData{
		primaryKey:       key,
		recoveryKey:      recoveryKey,
		recoveryKeyTries: 0,
		keyData:          keyData,
		model: testutil.MakeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij"),
		activateTries: 0,
	}), ErrorMatches, "cannot activate with platform protected keys:\n"+
		"- foo: snap model is not authorized\n"+
		"and activation with recovery key failed: no recovery key tries permitted")
}

type testActivateVolumeWithMultipleKeyDataData struct {
	keys    []DiskUnlockKey
	keyData []*KeyData

	volumeName       string
	sourceDevicePath string
	passphraseTries  int
	keyringPrefix    string

	model SnapModel

	authResponses []interface{}

	activateTries int

	key    DiskUnlockKey
	auxKey AuxiliaryKey
}

func (s *cryptSuite) testActivateVolumeWithMultipleKeyData(c *C, data *testActivateVolumeWithMultipleKeyDataData) {
	for _, k := range data.keys {
		s.addMockKeyslot(data.sourceDevicePath, k)
	}

	authRequestor := &mockAuthRequestor{passphraseResponses: data.authResponses}

	var kdf mockKDF
	options := &ActivateVolumeOptions{
		PassphraseTries: data.passphraseTries,
		KeyringPrefix:   data.keyringPrefix,
		Model:           data.model}
	err := ActivateVolumeWithMultipleKeyData(data.volumeName, data.sourceDevicePath, data.keyData, authRequestor, &kdf, options)
	c.Assert(err, IsNil)

	c.Check(authRequestor.passphraseRequests, HasLen, len(data.authResponses))
	for _, rsp := range authRequestor.passphraseRequests {
		c.Check(rsp.volumeName, Equals, data.volumeName)
		c.Check(rsp.sourceDevicePath, Equals, data.sourceDevicePath)
	}

	c.Assert(s.luks2.operations, HasLen, data.activateTries)
	for _, op := range s.luks2.operations {
		c.Check(op, Equals, "Activate("+data.volumeName+","+data.sourceDevicePath+")")
	}

	// This should be done last because it may fail in some circumstances.
	s.checkKeyDataKeysInKeyring(c, data.keyringPrefix, data.sourceDevicePath, data.key, data.auxKey)
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyData1(c *C) {
	keyData, keys, auxKeys := s.newMultipleNamedKeyData(c, "", "")

	models := []SnapModel{
		testutil.MakeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}
	c.Check(keyData[0].SetAuthorizedSnapModels(auxKeys[0], models...), IsNil)
	c.Check(keyData[1].SetAuthorizedSnapModels(auxKeys[1], models...), IsNil)

	s.testActivateVolumeWithMultipleKeyData(c, &testActivateVolumeWithMultipleKeyDataData{
		keys:             keys,
		keyData:          keyData,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		model:            models[0],
		activateTries:    1,
		key:              keys[0],
		auxKey:           auxKeys[0]})
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyData2(c *C) {
	// Test with a different volumeName / sourceDevicePath
	keyData, keys, auxKeys := s.newMultipleNamedKeyData(c, "", "")

	models := []SnapModel{
		testutil.MakeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}
	c.Check(keyData[0].SetAuthorizedSnapModels(auxKeys[0], models...), IsNil)
	c.Check(keyData[1].SetAuthorizedSnapModels(auxKeys[1], models...), IsNil)

	s.testActivateVolumeWithMultipleKeyData(c, &testActivateVolumeWithMultipleKeyDataData{
		keys:             keys,
		keyData:          keyData,
		volumeName:       "foo",
		sourceDevicePath: "/dev/vda2",
		model:            models[0],
		activateTries:    1,
		key:              keys[0],
		auxKey:           auxKeys[0]})
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyData3(c *C) {
	// Try with an invalid first key - the second key should be used for activation.
	keyData, keys, auxKeys := s.newMultipleNamedKeyData(c, "", "")

	models := []SnapModel{
		testutil.MakeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}
	c.Check(keyData[0].SetAuthorizedSnapModels(auxKeys[0], models...), IsNil)
	c.Check(keyData[1].SetAuthorizedSnapModels(auxKeys[1], models...), IsNil)

	s.testActivateVolumeWithMultipleKeyData(c, &testActivateVolumeWithMultipleKeyDataData{
		keys:             keys[1:],
		keyData:          keyData,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		model:            models[0],
		activateTries:    2,
		key:              keys[1],
		auxKey:           auxKeys[1]})
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyData4(c *C) {
	// Test with 2 keys that have a passphrase set, using the first key for activation.
	keyData, keys, auxKeys := s.newMultipleNamedKeyData(c, "", "")

	var kdf mockKDF
	c.Check(keyData[0].SetPassphrase("1234", nil, &kdf), IsNil)
	c.Check(keyData[1].SetPassphrase("5678", nil, &kdf), IsNil)

	models := []SnapModel{
		testutil.MakeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}
	c.Check(keyData[0].SetAuthorizedSnapModels(auxKeys[0], models...), IsNil)
	c.Check(keyData[1].SetAuthorizedSnapModels(auxKeys[1], models...), IsNil)

	s.testActivateVolumeWithMultipleKeyData(c, &testActivateVolumeWithMultipleKeyDataData{
		keys:             keys,
		keyData:          keyData,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		passphraseTries:  1,
		authResponses:    []interface{}{"1234"},
		model:            models[0],
		activateTries:    1,
		key:              keys[0],
		auxKey:           auxKeys[0]})
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyData5(c *C) {
	// Test with 2 keys that have a passphrase set, using the second key for activation.
	keyData, keys, auxKeys := s.newMultipleNamedKeyData(c, "", "")

	var kdf mockKDF
	c.Check(keyData[0].SetPassphrase("1234", nil, &kdf), IsNil)
	c.Check(keyData[1].SetPassphrase("5678", nil, &kdf), IsNil)

	models := []SnapModel{
		testutil.MakeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}
	c.Check(keyData[0].SetAuthorizedSnapModels(auxKeys[0], models...), IsNil)
	c.Check(keyData[1].SetAuthorizedSnapModels(auxKeys[1], models...), IsNil)

	s.testActivateVolumeWithMultipleKeyData(c, &testActivateVolumeWithMultipleKeyDataData{
		keys:             keys,
		keyData:          keyData,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		passphraseTries:  1,
		authResponses:    []interface{}{"5678"},
		model:            models[0],
		activateTries:    1,
		key:              keys[1],
		auxKey:           auxKeys[1]})
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyData6(c *C) {
	// Test with 2 keys where one has a passphrase set. The one without the passphrase
	// should be used first.
	keyData, keys, auxKeys := s.newMultipleNamedKeyData(c, "", "")

	var kdf mockKDF
	c.Check(keyData[0].SetPassphrase("1234", nil, &kdf), IsNil)

	models := []SnapModel{
		testutil.MakeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}
	c.Check(keyData[0].SetAuthorizedSnapModels(auxKeys[0], models...), IsNil)
	c.Check(keyData[1].SetAuthorizedSnapModels(auxKeys[1], models...), IsNil)

	s.testActivateVolumeWithMultipleKeyData(c, &testActivateVolumeWithMultipleKeyDataData{
		keys:             keys,
		keyData:          keyData,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		passphraseTries:  1,
		model:            models[0],
		activateTries:    1,
		key:              keys[1],
		auxKey:           auxKeys[1]})
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyData7(c *C) {
	// Test with 2 keys that have a passphrase set, using the second key for activation
	// after more than one attempt.
	keyData, keys, auxKeys := s.newMultipleNamedKeyData(c, "", "")

	var kdf mockKDF
	c.Check(keyData[0].SetPassphrase("1234", nil, &kdf), IsNil)
	c.Check(keyData[1].SetPassphrase("5678", nil, &kdf), IsNil)

	models := []SnapModel{
		testutil.MakeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}
	c.Check(keyData[0].SetAuthorizedSnapModels(auxKeys[0], models...), IsNil)
	c.Check(keyData[1].SetAuthorizedSnapModels(auxKeys[1], models...), IsNil)

	s.testActivateVolumeWithMultipleKeyData(c, &testActivateVolumeWithMultipleKeyDataData{
		keys:             keys,
		keyData:          keyData,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		passphraseTries:  3,
		authResponses:    []interface{}{"incorrect", "5678"},
		model:            models[0],
		activateTries:    1,
		key:              keys[1],
		auxKey:           auxKeys[1]})
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyData8(c *C) {
	// Test with 2 keys where one has a passphrase set. Activation fails with
	// the key that doesn't have a passphrase set, so activation should happen
	// with the key that has a passphrase set.
	keyData, keys, auxKeys := s.newMultipleNamedKeyData(c, "", "")

	var kdf mockKDF
	c.Check(keyData[1].SetPassphrase("5678", nil, &kdf), IsNil)

	models := []SnapModel{
		testutil.MakeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}
	c.Check(keyData[0].SetAuthorizedSnapModels(auxKeys[0], models...), IsNil)
	c.Check(keyData[1].SetAuthorizedSnapModels(auxKeys[1], models...), IsNil)

	s.testActivateVolumeWithMultipleKeyData(c, &testActivateVolumeWithMultipleKeyDataData{
		keys:             keys[1:],
		keyData:          keyData,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		passphraseTries:  1,
		authResponses:    []interface{}{"5678"},
		model:            models[0],
		activateTries:    2,
		key:              keys[1],
		auxKey:           auxKeys[1]})
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyData9(c *C) {
	// Try where the supplied model cannot be authorized via the first key - the
	// second key should be used for activation.
	keyData, keys, auxKeys := s.newMultipleNamedKeyData(c, "", "")

	models := []SnapModel{
		testutil.MakeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij"),
		testutil.MakeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "other-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij"),
	}
	c.Check(keyData[0].SetAuthorizedSnapModels(auxKeys[0], models[0]), IsNil)
	c.Check(keyData[1].SetAuthorizedSnapModels(auxKeys[1], models[1]), IsNil)

	s.testActivateVolumeWithMultipleKeyData(c, &testActivateVolumeWithMultipleKeyDataData{
		keys:             keys,
		keyData:          keyData,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		model:            models[1],
		activateTries:    1,
		key:              keys[1],
		auxKey:           auxKeys[1]})
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyData10(c *C) {
	keyData, keys, auxKeys := s.newMultipleNamedKeyData(c, "", "")

	models := []SnapModel{
		testutil.MakeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij")}
	c.Check(keyData[0].SetAuthorizedSnapModels(auxKeys[0], models...), IsNil)
	c.Check(keyData[1].SetAuthorizedSnapModels(auxKeys[1], models...), IsNil)

	s.testActivateVolumeWithMultipleKeyData(c, &testActivateVolumeWithMultipleKeyDataData{
		keys:             keys,
		keyData:          keyData,
		volumeName:       "data",
		sourceDevicePath: "/dev/sda1",
		model:            SkipSnapModelCheck,
		activateTries:    1,
		key:              keys[0],
		auxKey:           auxKeys[0]})
}

type testActivateVolumeWithMultipleKeyDataErrorHandlingData struct {
	keys        []DiskUnlockKey
	recoveryKey RecoveryKey
	keyData     []*KeyData

	authRequestor *mockAuthRequestor
	kdf           KDF

	passphraseTries  int
	recoveryKeyTries int
	keyringPrefix    string

	model SnapModel

	activateTries int
}

func (s *cryptSuite) testActivateVolumeWithMultipleKeyDataErrorHandling(c *C, data *testActivateVolumeWithMultipleKeyDataErrorHandlingData) error {
	for _, key := range data.keys {
		s.addMockKeyslot("/dev/sda1", key)
	}
	s.addMockKeyslot("/dev/sda1", data.recoveryKey[:])

	var authRequestor AuthRequestor
	var numPassphraseResponses int
	var numRecoveryResponses int
	if data.authRequestor != nil {
		authRequestor = data.authRequestor
		numPassphraseResponses = len(data.authRequestor.passphraseResponses)
		numRecoveryResponses = len(data.authRequestor.recoveryKeyResponses)
	}

	options := &ActivateVolumeOptions{
		PassphraseTries:  data.passphraseTries,
		RecoveryKeyTries: data.recoveryKeyTries,
		KeyringPrefix:    data.keyringPrefix,
		Model:            data.model}
	err := ActivateVolumeWithMultipleKeyData("data", "/dev/sda1", data.keyData, authRequestor, data.kdf, options)

	if data.authRequestor != nil {
		c.Check(data.authRequestor.passphraseRequests, HasLen, numPassphraseResponses)
		for _, rsp := range data.authRequestor.passphraseRequests {
			c.Check(rsp.volumeName, Equals, "data")
			c.Check(rsp.sourceDevicePath, Equals, "/dev/sda1")
		}

		c.Check(data.authRequestor.recoveryKeyRequests, HasLen, numRecoveryResponses)
		for _, rsp := range data.authRequestor.recoveryKeyRequests {
			c.Check(rsp.volumeName, Equals, "data")
			c.Check(rsp.sourceDevicePath, Equals, "/dev/sda1")
		}
	}

	c.Assert(s.luks2.operations, HasLen, data.activateTries)
	for _, op := range s.luks2.operations {
		c.Check(op, Equals, "Activate(data,/dev/sda1)")
	}

	if err == ErrRecoveryKeyUsed {
		// This should be done last because it may fail in some circumstances.
		s.checkRecoveryKeyInKeyring(c, data.keyringPrefix, "/dev/sda1", data.recoveryKey)
	}

	return err
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling1(c *C) {
	// Test with an invalid value for RecoveryKeyTries.
	keyData, _, _ := s.newMultipleNamedKeyData(c, "", "")

	c.Check(s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keyData:          keyData,
		recoveryKeyTries: -1,
	}), ErrorMatches, "invalid RecoveryKeyTries")
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling2(c *C) {
	// Test that recovery fallback works with the platform is unavailable
	keyData, keys, _ := s.newMultipleNamedKeyData(c, "", "")
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	c.Check(s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keys:             keys,
		recoveryKey:      recoveryKey,
		keyData:          keyData,
		authRequestor:    &mockAuthRequestor{recoveryKeyResponses: []interface{}{recoveryKey}},
		model:            SkipSnapModelCheck,
		recoveryKeyTries: 1,
		activateTries:    1,
	}), Equals, ErrRecoveryKeyUsed)
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling3(c *C) {
	// Test that recovery fallback works when the platform device isn't properly initialized
	keyData, keys, _ := s.newMultipleNamedKeyData(c, "", "")
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUninitialized

	c.Check(s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keys:             keys,
		recoveryKey:      recoveryKey,
		keyData:          keyData,
		authRequestor:    &mockAuthRequestor{recoveryKeyResponses: []interface{}{recoveryKey}},
		model:            SkipSnapModelCheck,
		recoveryKeyTries: 1,
		activateTries:    1,
	}), Equals, ErrRecoveryKeyUsed)
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling4(c *C) {
	// Test that recovery fallback works when the recovered key is incorrect
	keyData, _, _ := s.newMultipleNamedKeyData(c, "", "")
	recoveryKey := s.newRecoveryKey()

	c.Check(s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		recoveryKey:      recoveryKey,
		keyData:          keyData,
		authRequestor:    &mockAuthRequestor{recoveryKeyResponses: []interface{}{recoveryKey}},
		model:            SkipSnapModelCheck,
		recoveryKeyTries: 1,
		activateTries:    3,
	}), Equals, ErrRecoveryKeyUsed)
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling5(c *C) {
	// Test that activation fails if RecoveryKeyTries is zero.
	keyData, keys, _ := s.newMultipleNamedKeyData(c, "foo", "bar")
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	c.Check(s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keys:             keys,
		recoveryKey:      recoveryKey,
		keyData:          keyData,
		recoveryKeyTries: 0,
		model:            SkipSnapModelCheck,
		activateTries:    0,
	}), ErrorMatches,
		"cannot activate with platform protected keys:\n"+
			"- foo: cannot recover key: the platform's secure device is unavailable: the "+
			"platform device is unavailable\n"+
			"- bar: cannot recover key: the platform's secure device is unavailable: the "+
			"platform device is unavailable\n"+
			"and activation with recovery key failed: no recovery key tries permitted")
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling6(c *C) {
	// Test that activation fails if the supplied recovery key is incorrect
	keyData, keys, _ := s.newMultipleNamedKeyData(c, "bar", "foo")
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	c.Check(s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keys:             keys,
		recoveryKey:      recoveryKey,
		keyData:          keyData,
		authRequestor:    &mockAuthRequestor{recoveryKeyResponses: []interface{}{RecoveryKey{}}},
		recoveryKeyTries: 1,
		model:            SkipSnapModelCheck,
		activateTries:    1,
	}), ErrorMatches,
		"cannot activate with platform protected keys:\n"+
			"- bar: cannot recover key: the platform's secure device is unavailable: the "+
			"platform device is unavailable\n"+
			"- foo: cannot recover key: the platform's secure device is unavailable: the "+
			"platform device is unavailable\n"+
			"and activation with recovery key failed: cannot activate volume: "+
			"systemd-cryptsetup failed with: exit status 1")
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling7(c *C) {
	// Test that recovery fallback works if the correct key is eventually supplied
	keyData, keys, _ := s.newMultipleNamedKeyData(c, "", "")
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	c.Check(s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keys:             keys,
		recoveryKey:      recoveryKey,
		keyData:          keyData,
		authRequestor:    &mockAuthRequestor{recoveryKeyResponses: []interface{}{RecoveryKey{}, recoveryKey}},
		model:            SkipSnapModelCheck,
		recoveryKeyTries: 2,
		activateTries:    2,
	}), Equals, ErrRecoveryKeyUsed)
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling8(c *C) {
	// Test that recovery fallback works if the correct key is eventually supplied
	keyData, keys, _ := s.newMultipleNamedKeyData(c, "", "")
	recoveryKey := s.newRecoveryKey()

	s.handler.state = mockPlatformDeviceStateUnavailable

	c.Check(s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keys:             keys,
		recoveryKey:      recoveryKey,
		keyData:          keyData,
		authRequestor:    &mockAuthRequestor{recoveryKeyResponses: []interface{}{errors.New("some error"), recoveryKey}},
		model:            SkipSnapModelCheck,
		recoveryKeyTries: 2,
		activateTries:    1,
	}), Equals, ErrRecoveryKeyUsed)
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling9(c *C) {
	// Test that we get an error if no AuthRequestor is supplied when
	// RecoveryKeyTries is non-zero.
	keyData, _, _ := s.newMultipleNamedKeyData(c, "", "")

	c.Check(s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keyData:          keyData,
		recoveryKeyTries: 1,
		model:            SkipSnapModelCheck,
	}), ErrorMatches, "nil authRequestor")
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling10(c *C) {
	// Test that recovery key fallback works if the wrong passphrase is supplied.
	keyData, keys, _ := s.newMultipleNamedKeyData(c, "foo", "bar")
	recoveryKey := s.newRecoveryKey()

	var kdf mockKDF
	c.Check(keyData[0].SetPassphrase("1234", nil, &kdf), IsNil)
	c.Check(keyData[1].SetPassphrase("1234", nil, &kdf), IsNil)

	c.Check(s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keys:        keys,
		recoveryKey: recoveryKey,
		keyData:     keyData,
		authRequestor: &mockAuthRequestor{
			passphraseResponses:  []interface{}{"incorrect", "invalid"},
			recoveryKeyResponses: []interface{}{recoveryKey}},
		kdf:              &kdf,
		passphraseTries:  2,
		recoveryKeyTries: 1,
		model:            SkipSnapModelCheck,
		activateTries:    1,
	}), Equals, ErrRecoveryKeyUsed)
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling11(c *C) {
	// Test that we get an error if no AuthRequestor is supplied when
	// PassphraseTries is non-zero.
	keyData, _, _ := s.newMultipleNamedKeyData(c, "", "")

	c.Check(s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keyData:         keyData,
		passphraseTries: 1,
		model:           SkipSnapModelCheck,
	}), ErrorMatches, "nil authRequestor")
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling12(c *C) {
	// Test that we get an error if no KDF is supplied when
	// PassphraseTries is non-zero.
	keyData, _, _ := s.newMultipleNamedKeyData(c, "", "")

	c.Check(s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keyData:         keyData,
		authRequestor:   &mockAuthRequestor{},
		passphraseTries: 1,
		model:           SkipSnapModelCheck,
	}), ErrorMatches, "nil kdf")
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling13(c *C) {
	// Test that activation fails if the supplied passphrase and recovery key are incorrect
	keyData, keys, _ := s.newMultipleNamedKeyData(c, "foo", "bar")
	recoveryKey := s.newRecoveryKey()

	var kdf mockKDF
	c.Check(keyData[0].SetPassphrase("1234", nil, &kdf), IsNil)
	c.Check(keyData[1].SetPassphrase("1234", nil, &kdf), IsNil)

	c.Check(s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keys:        keys,
		recoveryKey: recoveryKey,
		keyData:     keyData,
		authRequestor: &mockAuthRequestor{
			passphraseResponses:  []interface{}{""},
			recoveryKeyResponses: []interface{}{RecoveryKey{}}},
		kdf:              &kdf,
		passphraseTries:  1,
		recoveryKeyTries: 1,
		model:            SkipSnapModelCheck,
		activateTries:    1,
	}), ErrorMatches, "cannot activate with platform protected keys:\n"+
		"- foo: cannot recover key: the supplied passphrase is incorrect\n"+
		"- bar: cannot recover key: the supplied passphrase is incorrect\n"+
		"and activation with recovery key failed: cannot activate volume: "+
		"systemd-cryptsetup failed with: exit status 1")
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling14(c *C) {
	// Test with an invalid value for SnapModel.
	keyData, _, _ := s.newMultipleNamedKeyData(c, "", "")

	c.Check(s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keyData: keyData,
	}), ErrorMatches, "nil Model")
}

func (s *cryptSuite) TestActivateVolumeWithMultipleKeyDataErrorHandling15(c *C) {
	// Test with an unauthorized snap model.
	keyData, keys, _ := s.newMultipleNamedKeyData(c, "foo", "bar")
	recoveryKey := s.newRecoveryKey()

	c.Check(s.testActivateVolumeWithMultipleKeyDataErrorHandling(c, &testActivateVolumeWithMultipleKeyDataErrorHandlingData{
		keys:             keys,
		recoveryKey:      recoveryKey,
		keyData:          keyData,
		recoveryKeyTries: 0,
		model: testutil.MakeMockCore20ModelAssertion(c, map[string]interface{}{
			"authority-id": "fake-brand",
			"series":       "16",
			"brand-id":     "fake-brand",
			"model":        "fake-model",
			"grade":        "secured",
		}, "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij"),
		activateTries: 0,
	}), ErrorMatches, "cannot activate with platform protected keys:\n"+
		"- foo: snap model is not authorized\n"+
		"- bar: snap model is not authorized\n"+
		"and activation with recovery key failed: no recovery key tries permitted")
}

type testActivateVolumeWithKeyData struct {
	keyData         []byte
	expectedKeyData []byte
	errMatch        string
	cmdCalled       bool
}

func (s *cryptSuite) testActivateVolumeWithKey(c *C, data *testActivateVolumeWithKeyData) {
	c.Assert(data.keyData, NotNil)

	expectedKeyData := data.expectedKeyData
	if expectedKeyData == nil {
		expectedKeyData = data.keyData
	}
	s.addMockKeyslot("/dev/sda1", expectedKeyData)

	options := ActivateVolumeOptions{}
	err := ActivateVolumeWithKey("luks-volume", "/dev/sda1", data.keyData, &options)
	if data.errMatch == "" {
		c.Check(err, IsNil)
	} else {
		c.Check(err, ErrorMatches, data.errMatch)
	}

	if data.cmdCalled {
		c.Check(s.luks2.operations, DeepEquals, []string{"Activate(luks-volume,/dev/sda1)"})
	} else {
		c.Check(s.luks2.operations, HasLen, 0)
	}
}

func (s *cryptSuite) TestActivateVolumeWithKey(c *C) {
	s.testActivateVolumeWithKey(c, &testActivateVolumeWithKeyData{
		keyData:   []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		cmdCalled: true,
	})
}

func (s *cryptSuite) TestActivateVolumeWithKeyMismatchErr(c *C) {
	s.testActivateVolumeWithKey(c, &testActivateVolumeWithKeyData{
		keyData:         []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		expectedKeyData: []byte{0, 0, 0, 0, 1},
		errMatch:        "systemd-cryptsetup failed with: exit status 1",
		cmdCalled:       true,
	})
}

func (s *cryptSuite) TestDeactivateVolume(c *C) {
	s.luks2.activated["luks-volume"] = "/dev/sda1"
	err := DeactivateVolume("luks-volume")
	c.Assert(err, IsNil)
	c.Check(s.luks2.operations, DeepEquals, []string{"Deactivate(luks-volume)"})
}

func (s *cryptSuite) TestDeactivateVolumeErr(c *C) {
	err := DeactivateVolume("bad-volume")
	c.Assert(err, ErrorMatches, `systemd-cryptsetup failed with: exit status 1`)
	c.Check(s.luks2.operations, DeepEquals, []string{"Deactivate(bad-volume)"})
}

type testInitializeLUKS2ContainerData struct {
	devicePath string
	label      string
	key        []byte
	opts       *InitializeLUKS2ContainerOptions

	fmtOpts *luks2.FormatOptions
}

func (s *cryptSuite) testInitializeLUKS2Container(c *C, data *testInitializeLUKS2ContainerData) {
	keyslotName := "default"
	if data.opts != nil && data.opts.InitialKeyslotName != "" {
		keyslotName = data.opts.InitialKeyslotName
	}

	c.Check(InitializeLUKS2Container(data.devicePath, data.label, data.key, data.opts), IsNil)

	c.Check(s.luks2.operations, DeepEquals, []string{
		fmt.Sprint("Format(", data.devicePath, ",", data.label, ",", data.fmtOpts, ")"),
		"ImportToken(" + data.devicePath + ",<nil>)",
		"SetSlotPriority(" + data.devicePath + ",0,prefer)"})

	dev, ok := s.luks2.devices[data.devicePath]
	c.Assert(ok, testutil.IsTrue)

	key, ok := dev.keyslots[0]
	c.Check(ok, testutil.IsTrue)
	c.Check(key, DeepEquals, data.key)

	var expectedToken luks2.Token = &luksview.KeyDataToken{
		TokenBase: luksview.TokenBase{
			TokenKeyslot: 0,
			TokenName:    keyslotName}}
	c.Check(dev.tokens[0], DeepEquals, expectedToken)
}

func (s *cryptSuite) TestInitializeLUKS2Container(c *C) {
	s.testInitializeLUKS2Container(c, &testInitializeLUKS2ContainerData{
		devicePath: "/dev/sda1",
		label:      "data",
		key:        s.newPrimaryKey(),
		fmtOpts:    &luks2.FormatOptions{KDFOptions: luks2.KDFOptions{ForceIterations: 4, MemoryKiB: 32}},
	})
}

func (s *cryptSuite) TestInitializeLUKS2ContainerDifferentArgs(c *C) {
	s.testInitializeLUKS2Container(c, &testInitializeLUKS2ContainerData{
		devicePath: "/dev/vdc2",
		label:      "test",
		key:        s.newPrimaryKey(),
		fmtOpts:    &luks2.FormatOptions{KDFOptions: luks2.KDFOptions{ForceIterations: 4, MemoryKiB: 32}},
	})
}

func (s *cryptSuite) TestInitializeLUKS2ContainerWithOptions(c *C) {
	s.testInitializeLUKS2Container(c, &testInitializeLUKS2ContainerData{
		devicePath: "/dev/sda1",
		label:      "data",
		key:        s.newPrimaryKey(),
		opts:       &InitializeLUKS2ContainerOptions{},
		fmtOpts:    &luks2.FormatOptions{KDFOptions: luks2.KDFOptions{ForceIterations: 4, MemoryKiB: 32}},
	})
}

func (s *cryptSuite) TestInitializeLUKS2ContainerWithCustomInitialKeyslotName(c *C) {
	s.testInitializeLUKS2Container(c, &testInitializeLUKS2ContainerData{
		devicePath: "/dev/sda1",
		label:      "data",
		key:        s.newPrimaryKey(),
		opts:       &InitializeLUKS2ContainerOptions{InitialKeyslotName: "foo"},
		fmtOpts:    &luks2.FormatOptions{KDFOptions: luks2.KDFOptions{ForceIterations: 4, MemoryKiB: 32}},
	})
}

func (s *cryptSuite) TestInitializeLUKS2ContainerWithCustomMetadataSize(c *C) {
	s.testInitializeLUKS2Container(c, &testInitializeLUKS2ContainerData{
		devicePath: "/dev/sda1",
		label:      "data",
		key:        s.newPrimaryKey(),
		opts: &InitializeLUKS2ContainerOptions{
			MetadataKiBSize:     2 * 1024, // 2MiB
			KeyslotsAreaKiBSize: 3 * 1024, // 3MiB
		},
		fmtOpts: &luks2.FormatOptions{
			MetadataKiBSize:     2 * 1024,
			KeyslotsAreaKiBSize: 3 * 1024,
			KDFOptions:          luks2.KDFOptions{ForceIterations: 4, MemoryKiB: 32},
		},
	})
}

func (s *cryptSuite) TestInitializeLUKS2ContainerWithCustomKDFTime(c *C) {
	s.testInitializeLUKS2Container(c, &testInitializeLUKS2ContainerData{
		devicePath: "/dev/sda1",
		label:      "data",
		key:        s.newPrimaryKey(),
		opts: &InitializeLUKS2ContainerOptions{
			KDFOptions: &KDFOptions{TargetDuration: 100 * time.Millisecond},
		},
		fmtOpts: &luks2.FormatOptions{KDFOptions: luks2.KDFOptions{TargetDuration: 100 * time.Millisecond}},
	})
}

func (s *cryptSuite) TestInitializeLUKS2ContainerWithCustomKDFMemory(c *C) {
	s.testInitializeLUKS2Container(c, &testInitializeLUKS2ContainerData{
		devicePath: "/dev/sda1",
		label:      "data",
		key:        s.newPrimaryKey(),
		opts: &InitializeLUKS2ContainerOptions{
			KDFOptions: &KDFOptions{MemoryKiB: 128},
		},
		fmtOpts: &luks2.FormatOptions{KDFOptions: luks2.KDFOptions{MemoryKiB: 128}},
	})
}

func (s *cryptSuite) TestInitializeLUKS2ContainerWithCustomKDFIterations(c *C) {
	s.testInitializeLUKS2Container(c, &testInitializeLUKS2ContainerData{
		devicePath: "/dev/sda1",
		label:      "data",
		key:        s.newPrimaryKey(),
		opts: &InitializeLUKS2ContainerOptions{
			KDFOptions: &KDFOptions{ForceIterations: 10},
		},
		fmtOpts: &luks2.FormatOptions{KDFOptions: luks2.KDFOptions{ForceIterations: 10}},
	})
}

func (s *cryptSuite) TestInitializeLUKS2ContainerInvalidKeySize(c *C) {
	c.Check(InitializeLUKS2Container("/dev/sda1", "data", s.newPrimaryKey()[0:16], nil), ErrorMatches, "expected a key length of at least 256-bits \\(got 128\\)")
}

type testAddLUKS2ContainerUnlockKeyData struct {
	devicePath  string
	dev         *mockLUKS2Container
	existingKey []byte
	key         []byte
	keyslotName string
	options     *KDFOptions

	expectedOptions *luks2.AddKeyOptions
	expectedTokenId int
}

func (s *cryptSuite) testAddLUKS2ContainerUnlockKey(c *C, data *testAddLUKS2ContainerUnlockKeyData) {
	s.luks2.devices[data.devicePath] = data.dev

	keyslotName := data.keyslotName
	if keyslotName == "" {
		keyslotName = "default"
	}
	options := data.options

	view, err := data.dev.newLUKSView()
	c.Assert(err, IsNil)

	expected := 4 + len(view.OrphanedTokenIds())

	c.Check(AddLUKS2ContainerUnlockKey(data.devicePath, data.keyslotName, data.existingKey, data.key, options), IsNil)

	c.Assert(s.luks2.operations, HasLen, expected)
	c.Check(s.luks2.operations[0], Equals, "newLUKSView("+data.devicePath+",0)")

	for i, id := range view.OrphanedTokenIds() {
		c.Check(s.luks2.operations[1+i], Equals, "RemoveToken("+data.devicePath+","+strconv.Itoa(id)+")")
	}

	c.Check(s.luks2.operations[len(view.OrphanedTokenIds())+1:], DeepEquals, []string{
		fmt.Sprint("AddKey(", data.devicePath, ",", data.expectedOptions, ")"),
		"ImportToken(" + data.devicePath + ",<nil>)",
		"SetSlotPriority(" + data.devicePath + "," + strconv.Itoa(data.expectedOptions.Slot) + ",prefer)",
	})

	key, ok := data.dev.keyslots[data.expectedOptions.Slot]
	c.Check(ok, testutil.IsTrue)
	c.Check(key, DeepEquals, data.key)

	var expectedToken luks2.Token = &luksview.KeyDataToken{
		TokenBase: luksview.TokenBase{
			TokenKeyslot: data.expectedOptions.Slot,
			TokenName:    keyslotName}}
	c.Check(data.dev.tokens[data.expectedTokenId], DeepEquals, expectedToken)
}

func (s *cryptSuite) TestAddLUKS2ContainerUnlockKey(c *C) {
	existingKey := s.newPrimaryKey()

	s.testAddLUKS2ContainerUnlockKey(c, &testAddLUKS2ContainerUnlockKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
			},
			keyslots: map[int][]byte{0: existingKey},
		},
		existingKey:     existingKey,
		key:             s.newPrimaryKey(),
		keyslotName:     "foo",
		expectedOptions: &luks2.AddKeyOptions{KDFOptions: luks2.KDFOptions{ForceIterations: 4, MemoryKiB: 32}, Slot: 1},
		expectedTokenId: 1,
	})
}

func (s *cryptSuite) TestAddLUKS2ContainerUnlockKeyDifferentPath(c *C) {
	existingKey := s.newPrimaryKey()

	s.testAddLUKS2ContainerUnlockKey(c, &testAddLUKS2ContainerUnlockKeyData{
		devicePath: "/dev/vdb1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
			},
			keyslots: map[int][]byte{0: existingKey},
		},
		existingKey:     existingKey,
		key:             s.newPrimaryKey(),
		keyslotName:     "foo",
		expectedOptions: &luks2.AddKeyOptions{KDFOptions: luks2.KDFOptions{ForceIterations: 4, MemoryKiB: 32}, Slot: 1},
		expectedTokenId: 1,
	})
}

func (s *cryptSuite) TestAddLUKS2ContainerUnlockKeyDifferentName(c *C) {
	existingKey := s.newPrimaryKey()

	s.testAddLUKS2ContainerUnlockKey(c, &testAddLUKS2ContainerUnlockKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
			},
			keyslots: map[int][]byte{0: existingKey},
		},
		existingKey:     existingKey,
		key:             s.newPrimaryKey(),
		keyslotName:     "bar",
		expectedOptions: &luks2.AddKeyOptions{KDFOptions: luks2.KDFOptions{ForceIterations: 4, MemoryKiB: 32}, Slot: 1},
		expectedTokenId: 1,
	})
}

func (s *cryptSuite) TestAddLUKS2ContainerUnlockKeyNoName(c *C) {
	existingKey := s.newPrimaryKey()

	s.testAddLUKS2ContainerUnlockKey(c, &testAddLUKS2ContainerUnlockKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "foo"}},
			},
			keyslots: map[int][]byte{0: existingKey},
		},
		existingKey:     existingKey,
		key:             s.newPrimaryKey(),
		expectedOptions: &luks2.AddKeyOptions{KDFOptions: luks2.KDFOptions{ForceIterations: 4, MemoryKiB: 32}, Slot: 1},
		expectedTokenId: 1,
	})
}

func (s *cryptSuite) TestAddLUKS2ContainerUnlockKeyWithOrphanedTokens(c *C) {
	existingKey := s.newPrimaryKey()

	s.testAddLUKS2ContainerUnlockKey(c, &testAddLUKS2ContainerUnlockKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
				1: luksview.MockOrphanedToken(luksview.KeyDataTokenType, "bar"),
			},
			keyslots: map[int][]byte{0: existingKey},
		},
		existingKey:     existingKey,
		key:             s.newPrimaryKey(),
		keyslotName:     "foo",
		expectedOptions: &luks2.AddKeyOptions{KDFOptions: luks2.KDFOptions{ForceIterations: 4, MemoryKiB: 32}, Slot: 1},
		expectedTokenId: 1,
	})
}

func (s *cryptSuite) TestAddLUKS2ContainerUnlockKeyWithExternalKeyslots(c *C) {
	existingKey := s.newPrimaryKey()

	s.testAddLUKS2ContainerUnlockKey(c, &testAddLUKS2ContainerUnlockKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
			},
			keyslots: map[int][]byte{
				0:  existingKey,
				1:  nil,
				2:  nil,
				3:  nil,
				10: nil,
			},
		},
		existingKey:     existingKey,
		key:             s.newPrimaryKey(),
		keyslotName:     "foo",
		expectedOptions: &luks2.AddKeyOptions{KDFOptions: luks2.KDFOptions{ForceIterations: 4, MemoryKiB: 32}, Slot: 4},
		expectedTokenId: 1,
	})
}

func (s *cryptSuite) TestAddLUKS2ContainerUnlockKeyWithCustomKDFTime(c *C) {
	existingKey := s.newPrimaryKey()

	s.testAddLUKS2ContainerUnlockKey(c, &testAddLUKS2ContainerUnlockKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
			},
			keyslots: map[int][]byte{0: existingKey},
		},
		existingKey:     existingKey,
		key:             s.newPrimaryKey(),
		keyslotName:     "foo",
		options:         &KDFOptions{TargetDuration: 100 * time.Millisecond},
		expectedOptions: &luks2.AddKeyOptions{KDFOptions: luks2.KDFOptions{TargetDuration: 100 * time.Millisecond}, Slot: 1},
		expectedTokenId: 1,
	})
}

func (s *cryptSuite) TestAddLUKS2ContainerUnlockKeyWithCustomKDFMemory(c *C) {
	existingKey := s.newPrimaryKey()

	s.testAddLUKS2ContainerUnlockKey(c, &testAddLUKS2ContainerUnlockKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
			},
			keyslots: map[int][]byte{0: existingKey},
		},
		existingKey:     existingKey,
		key:             s.newPrimaryKey(),
		keyslotName:     "foo",
		options:         &KDFOptions{MemoryKiB: 64},
		expectedOptions: &luks2.AddKeyOptions{KDFOptions: luks2.KDFOptions{MemoryKiB: 64}, Slot: 1},
		expectedTokenId: 1,
	})
}

func (s *cryptSuite) TestAddLUKS2ContainerUnlockKeyWithCustomKDFIterations(c *C) {
	existingKey := s.newPrimaryKey()

	s.testAddLUKS2ContainerUnlockKey(c, &testAddLUKS2ContainerUnlockKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
			},
			keyslots: map[int][]byte{0: existingKey},
		},
		existingKey:     existingKey,
		key:             s.newPrimaryKey(),
		keyslotName:     "foo",
		options:         &KDFOptions{ForceIterations: 10},
		expectedOptions: &luks2.AddKeyOptions{KDFOptions: luks2.KDFOptions{ForceIterations: 10}, Slot: 1},
		expectedTokenId: 1,
	})
}

func (s *cryptSuite) TestAddLUKS2ContainerUnlockKeyNameInUse(c *C) {
	existingKey := s.newPrimaryKey()

	s.luks2.devices["/dev/sda1"] = &mockLUKS2Container{
		tokens: map[int]luks2.Token{
			0: &luksview.KeyDataToken{
				TokenBase: luksview.TokenBase{
					TokenKeyslot: 0,
					TokenName:    "default"}},
			1: &luksview.RecoveryToken{
				TokenBase: luksview.TokenBase{
					TokenKeyslot: 1,
					TokenName:    "recovery"}},
		},
		keyslots: map[int][]byte{
			0: nil,
			1: existingKey,
		},
	}
	c.Check(AddLUKS2ContainerUnlockKey("/dev/sda1", "default", existingKey, make([]byte, 32), nil), ErrorMatches, "the specified name is already in use")
}

func (s *cryptSuite) TestListLUKS2ContainerKeyNames(c *C) {
	s.luks2.devices["/dev/sda1"] = &mockLUKS2Container{
		tokens: map[int]luks2.Token{
			0: &luksview.KeyDataToken{
				TokenBase: luksview.TokenBase{
					TokenKeyslot: 0,
					TokenName:    "default"}},
			1: &luksview.RecoveryToken{
				TokenBase: luksview.TokenBase{
					TokenKeyslot: 1,
					TokenName:    "recovery"}},
			2: &luksview.KeyDataToken{
				TokenBase: luksview.TokenBase{
					TokenKeyslot: 2,
					TokenName:    "foo"}},
			3: luksview.MockOrphanedToken(luksview.KeyDataTokenType, "orphaned"),
		},
		keyslots: map[int][]byte{
			0: nil,
			1: nil,
			2: nil,
		},
	}

	names, err := ListLUKS2ContainerUnlockKeyNames("/dev/sda1")
	c.Check(err, IsNil)
	c.Check(names, DeepEquals, []string{"default", "foo"})

	names, err = ListLUKS2ContainerRecoveryKeyNames("/dev/sda1")
	c.Check(err, IsNil)
	c.Check(names, DeepEquals, []string{"recovery"})

	c.Check(s.luks2.operations, DeepEquals, []string{
		"newLUKSView(/dev/sda1,0)",
		"newLUKSView(/dev/sda1,0)",
	})
}

type testAddLUKS2ContainerRecoveryKeyData struct {
	devicePath  string
	dev         *mockLUKS2Container
	existingKey []byte
	key         RecoveryKey
	keyslotName string
	options     *KDFOptions

	expectedOptions *luks2.AddKeyOptions
	expectedTokenId int
}

func (s *cryptSuite) testAddLUKS2ContainerRecoveryKey(c *C, data *testAddLUKS2ContainerRecoveryKeyData) {
	s.luks2.devices[data.devicePath] = data.dev

	keyslotName := data.keyslotName
	if keyslotName == "" {
		keyslotName = "default-recovery"
	}
	options := data.options

	view, err := data.dev.newLUKSView()
	c.Assert(err, IsNil)

	expected := 4 + len(view.OrphanedTokenIds())

	c.Check(AddLUKS2ContainerRecoveryKey(data.devicePath, data.keyslotName, data.existingKey, data.key, options), IsNil)

	c.Assert(s.luks2.operations, HasLen, expected)
	c.Check(s.luks2.operations[0], Equals, "newLUKSView("+data.devicePath+",0)")

	for i, id := range view.OrphanedTokenIds() {
		c.Check(s.luks2.operations[1+i], Equals, "RemoveToken("+data.devicePath+","+strconv.Itoa(id)+")")
	}

	c.Check(s.luks2.operations[len(view.OrphanedTokenIds())+1:], DeepEquals, []string{
		fmt.Sprint("AddKey(", data.devicePath, ",", data.expectedOptions, ")"),
		"ImportToken(" + data.devicePath + ",<nil>)",
		"SetSlotPriority(" + data.devicePath + "," + strconv.Itoa(data.expectedOptions.Slot) + ",normal)",
	})

	key, ok := data.dev.keyslots[data.expectedOptions.Slot]
	c.Check(ok, testutil.IsTrue)
	c.Check(key, DeepEquals, []byte(data.key[:]))

	var expectedToken luks2.Token = &luksview.RecoveryToken{
		TokenBase: luksview.TokenBase{
			TokenKeyslot: data.expectedOptions.Slot,
			TokenName:    keyslotName}}
	c.Check(data.dev.tokens[data.expectedTokenId], DeepEquals, expectedToken)
}

func (s *cryptSuite) TestAddLUKS2ContainerRecoveryKey(c *C) {
	existingKey := s.newPrimaryKey()

	s.testAddLUKS2ContainerRecoveryKey(c, &testAddLUKS2ContainerRecoveryKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
			},
			keyslots: map[int][]byte{0: existingKey},
		},
		existingKey:     existingKey,
		key:             s.newRecoveryKey(),
		keyslotName:     "recovery",
		expectedOptions: &luks2.AddKeyOptions{Slot: 1},
		expectedTokenId: 1,
	})
}

func (s *cryptSuite) TestAddLUKS2ContainerRecoveryKeyDifferentPath(c *C) {
	existingKey := s.newPrimaryKey()

	s.testAddLUKS2ContainerRecoveryKey(c, &testAddLUKS2ContainerRecoveryKeyData{
		devicePath: "/dev/vdb2",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
			},
			keyslots: map[int][]byte{0: existingKey},
		},
		existingKey:     existingKey,
		key:             s.newRecoveryKey(),
		keyslotName:     "recovery",
		expectedOptions: &luks2.AddKeyOptions{Slot: 1},
		expectedTokenId: 1,
	})
}

func (s *cryptSuite) TestAddLUKS2ContainerRecoveryKeyDifferentName(c *C) {
	existingKey := s.newPrimaryKey()

	s.testAddLUKS2ContainerRecoveryKey(c, &testAddLUKS2ContainerRecoveryKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
			},
			keyslots: map[int][]byte{0: existingKey},
		},
		existingKey:     existingKey,
		key:             s.newRecoveryKey(),
		keyslotName:     "foo",
		expectedOptions: &luks2.AddKeyOptions{Slot: 1},
		expectedTokenId: 1,
	})
}

func (s *cryptSuite) TestAddLUKS2ContainerRecoveryKeyNoName(c *C) {
	existingKey := s.newPrimaryKey()

	s.testAddLUKS2ContainerRecoveryKey(c, &testAddLUKS2ContainerRecoveryKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
			},
			keyslots: map[int][]byte{0: existingKey},
		},
		existingKey:     existingKey,
		key:             s.newRecoveryKey(),
		expectedOptions: &luks2.AddKeyOptions{Slot: 1},
		expectedTokenId: 1,
	})
}

func (s *cryptSuite) TestAddLUKS2ContainerRecoveryKeyWithOrphanedTokens(c *C) {
	existingKey := s.newPrimaryKey()

	s.testAddLUKS2ContainerRecoveryKey(c, &testAddLUKS2ContainerRecoveryKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
				1: luksview.MockOrphanedToken(luksview.RecoveryTokenType, "recovery"),
			},
			keyslots: map[int][]byte{0: existingKey},
		},
		existingKey:     existingKey,
		key:             s.newRecoveryKey(),
		keyslotName:     "recovery",
		expectedOptions: &luks2.AddKeyOptions{Slot: 1},
		expectedTokenId: 1,
	})
}

func (s *cryptSuite) TestAddLUKS2ContainerRecoveryKeyWithExternalKeyslots(c *C) {
	existingKey := s.newPrimaryKey()

	s.testAddLUKS2ContainerRecoveryKey(c, &testAddLUKS2ContainerRecoveryKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
			},
			keyslots: map[int][]byte{
				0: existingKey,
				1: nil,
				2: nil,
				3: nil,
				4: nil,
				5: nil,
				9: nil,
			},
		},
		existingKey:     existingKey,
		key:             s.newRecoveryKey(),
		keyslotName:     "recovery",
		expectedOptions: &luks2.AddKeyOptions{Slot: 6},
		expectedTokenId: 1,
	})
}

func (s *cryptSuite) TestAddLUKS2ContainerRecoveryKeyWithCustomKDFTime(c *C) {
	existingKey := s.newPrimaryKey()

	s.testAddLUKS2ContainerRecoveryKey(c, &testAddLUKS2ContainerRecoveryKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
			},
			keyslots: map[int][]byte{0: existingKey},
		},
		existingKey:     existingKey,
		key:             s.newRecoveryKey(),
		keyslotName:     "recovery",
		options:         &KDFOptions{TargetDuration: 5 * time.Second},
		expectedOptions: &luks2.AddKeyOptions{KDFOptions: luks2.KDFOptions{TargetDuration: 5 * time.Second}, Slot: 1},
		expectedTokenId: 1,
	})
}

func (s *cryptSuite) TestAddLUKS2ContainerRecoveryKeyWithCustomKDFMemory(c *C) {
	existingKey := s.newPrimaryKey()

	s.testAddLUKS2ContainerRecoveryKey(c, &testAddLUKS2ContainerRecoveryKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
			},
			keyslots: map[int][]byte{0: existingKey},
		},
		existingKey:     existingKey,
		key:             s.newRecoveryKey(),
		keyslotName:     "recovery",
		options:         &KDFOptions{MemoryKiB: 2 * 1024 * 1024},
		expectedOptions: &luks2.AddKeyOptions{KDFOptions: luks2.KDFOptions{MemoryKiB: 2 * 1024 * 1024}, Slot: 1},
		expectedTokenId: 1,
	})
}

func (s *cryptSuite) TestAddLUKS2ContainerRecoveryKeyWithCustomKDFIterations(c *C) {
	existingKey := s.newPrimaryKey()

	s.testAddLUKS2ContainerRecoveryKey(c, &testAddLUKS2ContainerRecoveryKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
			},
			keyslots: map[int][]byte{0: existingKey},
		},
		existingKey:     existingKey,
		key:             s.newRecoveryKey(),
		keyslotName:     "recovery",
		options:         &KDFOptions{ForceIterations: 10},
		expectedOptions: &luks2.AddKeyOptions{KDFOptions: luks2.KDFOptions{ForceIterations: 10}, Slot: 1},
		expectedTokenId: 1,
	})
}

func (s *cryptSuite) TestAddLUKS2ContainerRecoveryKeyDifferentSlot(c *C) {
	existingKey := s.newPrimaryKey()

	s.testAddLUKS2ContainerRecoveryKey(c, &testAddLUKS2ContainerRecoveryKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
			},
			keyslots: map[int][]byte{
				0: existingKey,
				1: nil,
			},
		},
		existingKey:     existingKey,
		key:             s.newRecoveryKey(),
		keyslotName:     "recovery",
		expectedOptions: &luks2.AddKeyOptions{Slot: 2},
		expectedTokenId: 1,
	})
}

func (s *cryptSuite) TestAddLUKS2ContainerRecoveryKeyNameInUse(c *C) {
	existingKey := s.newPrimaryKey()

	s.luks2.devices["/dev/sda1"] = &mockLUKS2Container{
		tokens: map[int]luks2.Token{
			0: &luksview.KeyDataToken{
				TokenBase: luksview.TokenBase{
					TokenKeyslot: 0,
					TokenName:    "default"}},
			1: &luksview.RecoveryToken{
				TokenBase: luksview.TokenBase{
					TokenKeyslot: 1,
					TokenName:    "recovery"}},
		},
		keyslots: map[int][]byte{
			0: existingKey,
			1: nil,
		},
	}
	c.Check(AddLUKS2ContainerRecoveryKey("/dev/sda1", "recovery", existingKey, RecoveryKey{}, nil), ErrorMatches, "the specified name is already in use")
}

type testDeleteLUKS2ContainerKeyData struct {
	devicePath  string
	dev         *mockLUKS2Container
	keyslotName string
	existingKey []byte
	slot        int
	tokenId     int
}

func (s *cryptSuite) testDeleteLUKS2ContainerKey(c *C, data *testDeleteLUKS2ContainerKeyData) {
	s.luks2.devices[data.devicePath] = data.dev

	c.Check(DeleteLUKS2ContainerKey(data.devicePath, data.keyslotName, data.existingKey), IsNil)

	c.Check(s.luks2.operations, DeepEquals, []string{
		"newLUKSView(" + data.devicePath + ",0)",
		"KillSlot(" + data.devicePath + "," + strconv.Itoa(data.slot) + ")",
		"RemoveToken(" + data.devicePath + "," + strconv.Itoa(data.tokenId) + ")",
	})
}

func (s *cryptSuite) TestDeleteLUKS2ContainerKey(c *C) {
	existingKey := s.newPrimaryKey()

	s.testDeleteLUKS2ContainerKey(c, &testDeleteLUKS2ContainerKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
				1: &luksview.RecoveryToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 1,
						TokenName:    "default-recovery"}},
			},
			keyslots: map[int][]byte{
				0: existingKey,
				1: nil,
			},
		},
		keyslotName: "default-recovery",
		existingKey: existingKey,
		slot:        1,
		tokenId:     1,
	})
}

func (s *cryptSuite) TestDeleteLUKS2ContainerKeyDifferentPath(c *C) {
	existingKey := s.newPrimaryKey()

	s.testDeleteLUKS2ContainerKey(c, &testDeleteLUKS2ContainerKeyData{
		devicePath: "/dev/nvme0n1p1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
				1: &luksview.RecoveryToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 1,
						TokenName:    "default-recovery"}},
			},
			keyslots: map[int][]byte{
				0: existingKey,
				1: nil,
			},
		},
		keyslotName: "default-recovery",
		existingKey: existingKey,
		slot:        1,
		tokenId:     1,
	})
}

func (s *cryptSuite) TestDeleteLUKS2ContainerKeyDifferentName(c *C) {
	existingKey := s.newPrimaryKey()

	s.testDeleteLUKS2ContainerKey(c, &testDeleteLUKS2ContainerKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
				1: &luksview.RecoveryToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 1,
						TokenName:    "foo"}},
			},
			keyslots: map[int][]byte{
				0: existingKey,
				1: nil,
			},
		},
		keyslotName: "foo",
		existingKey: existingKey,
		slot:        1,
		tokenId:     1,
	})
}

func (s *cryptSuite) TestDeleteLUKS2ContainerKeyDifferentKeyslot(c *C) {
	existingKey := s.newPrimaryKey()

	s.testDeleteLUKS2ContainerKey(c, &testDeleteLUKS2ContainerKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
				1: &luksview.RecoveryToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 2,
						TokenName:    "default-recovery"}},
			},
			keyslots: map[int][]byte{
				0: existingKey,
				2: nil,
			},
		},
		keyslotName: "default-recovery",
		existingKey: existingKey,
		slot:        2,
		tokenId:     1,
	})
}

func (s *cryptSuite) TestDeleteLUKS2ContainerKeyDifferentTokenId(c *C) {
	existingKey := s.newPrimaryKey()

	s.testDeleteLUKS2ContainerKey(c, &testDeleteLUKS2ContainerKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"}},
				4: &luksview.RecoveryToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 1,
						TokenName:    "default-recovery"}},
			},
			keyslots: map[int][]byte{
				0: existingKey,
				1: nil,
			},
		},
		keyslotName: "default-recovery",
		existingKey: existingKey,
		slot:        1,
		tokenId:     4,
	})
}

func (s *cryptSuite) TestDeleteLUKS2ContainerKeyLastSlot(c *C) {
	existingKey := s.newPrimaryKey()

	s.luks2.devices["/dev/sda1"] = &mockLUKS2Container{
		tokens: map[int]luks2.Token{
			0: &luksview.KeyDataToken{
				TokenBase: luksview.TokenBase{
					TokenKeyslot: 0,
					TokenName:    "default"}},
		},
		keyslots: map[int][]byte{0: existingKey},
	}

	c.Check(DeleteLUKS2ContainerKey("/dev/sda1", "default", existingKey), ErrorMatches, "cannot kill last remaining slot")
}

type testRenameLUKS2ContainerKeyData struct {
	devicePath string

	dev *mockLUKS2Container

	oldName string
	newName string

	tokenId       int
	expectedToken luks2.Token
}

func (s *cryptSuite) testRenameLUKS2ContainerKey(c *C, data *testRenameLUKS2ContainerKeyData) {
	s.luks2.devices[data.devicePath] = data.dev

	c.Check(RenameLUKS2ContainerKey(data.devicePath, data.oldName, data.newName), IsNil)

	c.Check(s.luks2.operations, DeepEquals, []string{
		"newLUKSView(" + data.devicePath + ",0)",
		fmt.Sprint("ImportToken(", data.devicePath, ",", &luks2.ImportTokenOptions{Id: data.tokenId, Replace: true}, ")"),
	})

	newToken, ok := data.dev.tokens[data.tokenId]
	c.Assert(ok, testutil.IsTrue)
	c.Check(newToken, DeepEquals, data.expectedToken)
}

func (s *cryptSuite) TestRenameLUKS2ContainerKeyKeyData(c *C) {
	s.testRenameLUKS2ContainerKey(c, &testRenameLUKS2ContainerKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "foo"},
					Priority: 10,
					Data:     json.RawMessage("1234567890")},
				1: &luksview.RecoveryToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 1,
						TokenName:    "default-recovery"}},
			},
			keyslots: map[int][]byte{
				0: nil,
				1: nil,
			},
		},
		oldName: "foo",
		newName: "bar",
		tokenId: 0,
		expectedToken: &luksview.KeyDataToken{
			TokenBase: luksview.TokenBase{
				TokenKeyslot: 0,
				TokenName:    "bar"},
			Priority: 10,
			Data:     json.RawMessage("1234567890")}})
}

func (s *cryptSuite) TestRenameLUKS2ContainerKeyRecovery(c *C) {
	s.testRenameLUKS2ContainerKey(c, &testRenameLUKS2ContainerKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "default"},
					Priority: 10,
					Data:     json.RawMessage("1234567890")},
				1: &luksview.RecoveryToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 1,
						TokenName:    "foo"}},
			},
			keyslots: map[int][]byte{
				0: nil,
				1: nil,
			},
		},
		oldName: "foo",
		newName: "bar",
		tokenId: 1,
		expectedToken: &luksview.RecoveryToken{
			TokenBase: luksview.TokenBase{
				TokenKeyslot: 1,
				TokenName:    "bar"}}})
}

func (s *cryptSuite) TestRenameLUKS2ContainerKeyDifferentPath(c *C) {
	s.testRenameLUKS2ContainerKey(c, &testRenameLUKS2ContainerKeyData{
		devicePath: "/dev/vdb2",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "foo"},
					Priority: 10,
					Data:     json.RawMessage("1234567890")},
				1: &luksview.RecoveryToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 1,
						TokenName:    "default-recovery"}},
			},
			keyslots: map[int][]byte{
				0: nil,
				1: nil,
			},
		},
		oldName: "foo",
		newName: "bar",
		tokenId: 0,
		expectedToken: &luksview.KeyDataToken{
			TokenBase: luksview.TokenBase{
				TokenKeyslot: 0,
				TokenName:    "bar"},
			Priority: 10,
			Data:     json.RawMessage("1234567890")}})
}

func (s *cryptSuite) TestRenameLUKS2ContainerKeyDifferentNames(c *C) {
	s.testRenameLUKS2ContainerKey(c, &testRenameLUKS2ContainerKeyData{
		devicePath: "/dev/sda1",
		dev: &mockLUKS2Container{
			tokens: map[int]luks2.Token{
				0: &luksview.KeyDataToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 0,
						TokenName:    "bar"},
					Priority: 10,
					Data:     json.RawMessage("1234567890")},
				1: &luksview.RecoveryToken{
					TokenBase: luksview.TokenBase{
						TokenKeyslot: 1,
						TokenName:    "default-recovery"}},
			},
			keyslots: map[int][]byte{
				0: nil,
				1: nil,
			},
		},
		oldName: "bar",
		newName: "foo",
		tokenId: 0,
		expectedToken: &luksview.KeyDataToken{
			TokenBase: luksview.TokenBase{
				TokenKeyslot: 0,
				TokenName:    "foo"},
			Priority: 10,
			Data:     json.RawMessage("1234567890")}})
}

func (s *cryptSuite) TestRenameLUKS2ContainerKeyNonExistant(c *C) {
	s.luks2.devices["/dev/sda1"] = &mockLUKS2Container{
		tokens: map[int]luks2.Token{
			0: &luksview.KeyDataToken{
				TokenBase: luksview.TokenBase{
					TokenKeyslot: 0,
					TokenName:    "foo"}},
		},
		keyslots: map[int][]byte{
			0: nil,
		},
	}

	c.Check(RenameLUKS2ContainerKey("/dev/sda1", "bar", "foo"), ErrorMatches, "no key with the specified name exists")
}

func (s *cryptSuite) TestRenameLUKS2ContainerKeyNameInUse(c *C) {
	s.luks2.devices["/dev/sda1"] = &mockLUKS2Container{
		tokens: map[int]luks2.Token{
			0: &luksview.KeyDataToken{
				TokenBase: luksview.TokenBase{
					TokenKeyslot: 0,
					TokenName:    "foo"}},
			1: &luksview.RecoveryToken{
				TokenBase: luksview.TokenBase{
					TokenKeyslot: 1,
					TokenName:    "bar"}},
		},
		keyslots: map[int][]byte{
			0: nil,
			1: nil,
		},
	}

	c.Check(RenameLUKS2ContainerKey("/dev/sda1", "foo", "bar"), ErrorMatches, "the new name is already in use")
}

type cryptSuiteUnmockedBase struct {
	snapd_testutil.BaseTest
	cryptTestBase
}

func (s *cryptSuiteUnmockedBase) SetUpSuite(c *C) {
	if luks2.DetectCryptsetupFeatures()&luks2.FeatureTokenImport == 0 {
		c.Skip("cryptsetup doesn't support token import")
	}
}

func (s *cryptSuiteUnmockedBase) SetUpTest(c *C) {
	s.BaseTest.SetUpTest(c)
	s.AddCleanup(pathstest.MockRunDir(c.MkDir()))
	s.AddCleanup(luks2test.WrapCryptsetup(c))
}

type cryptSuiteUnmocked struct {
	cryptSuiteUnmockedBase
}

type cryptSuiteUnmockedExpensive struct {
	cryptSuiteUnmockedBase
}

func (s *cryptSuiteUnmockedExpensive) SetUpSuite(c *C) {
	if _, exists := os.LookupEnv("NO_EXPENSIVE_CRYPTSETUP_TESTS"); exists {
		c.Skip("skipping expensive cryptsetup tests")
	}
	s.cryptSuiteUnmockedBase.SetUpSuite(c)
}

var _ = Suite(&cryptSuiteUnmocked{})
var _ = Suite(&cryptSuiteUnmockedExpensive{})

func (s *cryptSuiteUnmockedBase) testInitializeLUKS2Container(c *C, options *InitializeLUKS2ContainerOptions) {
	key := s.newPrimaryKey()
	path := luks2test.CreateEmptyDiskImage(c, 20)

	c.Check(InitializeLUKS2Container(path, "data", key, options), IsNil)

	if options == nil {
		options = &InitializeLUKS2ContainerOptions{}
	}
	if options.KDFOptions == nil {
		options.KDFOptions = &KDFOptions{MemoryKiB: 32, ForceIterations: 4}
	}

	info, err := luks2.ReadHeader(path, luks2.LockModeBlocking)
	c.Assert(err, IsNil)

	c.Check(info.Label, Equals, "data")

	c.Check(info.Metadata.Keyslots, HasLen, 1)
	keyslot, ok := info.Metadata.Keyslots[0]
	c.Assert(ok, Equals, true)
	c.Check(keyslot.Priority, Equals, luks2.SlotPriorityHigh)

	expectedName := "default"
	if options.InitialKeyslotName != "" {
		expectedName = options.InitialKeyslotName
	}

	c.Check(info.Metadata.Tokens, HasLen, 1)
	c.Check(info.Metadata.Tokens[0], DeepEquals, luks2.Token(&luksview.KeyDataToken{
		TokenBase: luksview.TokenBase{
			TokenKeyslot: 0,
			TokenName:    expectedName},
		Priority: 0}))

	expectedMetadataSize := uint64(16 * 1024)
	if options.MetadataKiBSize > 0 {
		expectedMetadataSize = uint64(options.MetadataKiBSize * 1024)
	}
	expectedKeyslotsSize := uint64(16*1024*1024) - (2 * expectedMetadataSize)
	if options.KeyslotsAreaKiBSize > 0 {
		expectedKeyslotsSize = uint64(options.KeyslotsAreaKiBSize * 1024)
	}

	c.Check(info.Metadata.Config.JSONSize, Equals, expectedMetadataSize-uint64(4*1024))
	c.Check(info.Metadata.Config.KeyslotsSize, Equals, expectedKeyslotsSize)

	expectedMemoryKiB := 1 * 1024 * 1024
	if options.KDFOptions.MemoryKiB > 0 {
		expectedMemoryKiB = options.KDFOptions.MemoryKiB
	}

	if options.KDFOptions.ForceIterations > 0 {
		c.Check(keyslot.KDF.Time, Equals, options.KDFOptions.ForceIterations)
		c.Check(keyslot.KDF.Memory, Equals, expectedMemoryKiB)
		luks2test.CheckLUKS2Passphrase(c, path, key)
	} else {
		expectedKDFTime := 2 * time.Second
		if options.KDFOptions.TargetDuration > 0 {
			expectedKDFTime = options.KDFOptions.TargetDuration
		}

		c.Check(keyslot.KDF.Memory, snapd_testutil.IntLessEqual, expectedMemoryKiB)

		start := time.Now()
		luks2test.CheckLUKS2Passphrase(c, path, key)
		elapsed := time.Now().Sub(start)

		// Check KDF time here with +/-20% tolerance and additional 500ms for cryptsetup exec and other activities
		c.Check(int(elapsed/time.Millisecond), snapd_testutil.IntGreaterThan, int(float64(expectedKDFTime/time.Millisecond)*0.8))
		c.Check(int(elapsed/time.Millisecond), snapd_testutil.IntLessThan, int(float64(expectedKDFTime/time.Millisecond)*1.2)+500)
	}
}

func (s *cryptSuiteUnmocked) TestInitializeLUKS2Container(c *C) {
	s.testInitializeLUKS2Container(c, nil)
}

func (s *cryptSuiteUnmocked) TestInitializeLUKS2ContainerWithCustomMetadataSize(c *C) {
	if luks2.DetectCryptsetupFeatures()&luks2.FeatureHeaderSizeSetting == 0 {
		c.Skip("cryptsetup doesn't support --luks2-metadata-size or --luks2-keyslots-size")
	}

	s.testInitializeLUKS2Container(c, &InitializeLUKS2ContainerOptions{
		MetadataKiBSize:     2 * 1024, // 2MiB
		KeyslotsAreaKiBSize: 3 * 1024, // 3MiB
		InitialKeyslotName:  "foo",
	})
}

func (s *cryptSuiteUnmocked) TestInitializeLUKS2ContainerWithCustomKeyslotName(c *C) {
	s.testInitializeLUKS2Container(c, &InitializeLUKS2ContainerOptions{
		InitialKeyslotName: "foo",
	})
}

func (s *cryptSuiteUnmockedExpensive) TestInitializeLUKS2ContainerWithCustomKDFTime(c *C) {
	s.testInitializeLUKS2Container(c, &InitializeLUKS2ContainerOptions{
		KDFOptions: &KDFOptions{TargetDuration: 100 * time.Millisecond}})
}

func (s *cryptSuiteUnmockedExpensive) TestInitializeLUKS2ContainerWithCustomKDFMemory(c *C) {
	s.testInitializeLUKS2Container(c, &InitializeLUKS2ContainerOptions{
		KDFOptions: &KDFOptions{MemoryKiB: 64}})
}

func (s *cryptSuiteUnmocked) TestInitializeLUKS2ContainerWithCustomKDFIterations(c *C) {
	s.testInitializeLUKS2Container(c, &InitializeLUKS2ContainerOptions{
		KDFOptions: &KDFOptions{MemoryKiB: 32, ForceIterations: 8}})
}

type testAddLUKS2ContainerUnlockKeyUnmockedData struct {
	keyslotName string
	options     *KDFOptions
}

func (s *cryptSuiteUnmockedBase) testAddLUKS2ContainerUnlockKey(c *C, data *testAddLUKS2ContainerUnlockKeyUnmockedData) {
	key := s.newPrimaryKey()
	path := luks2test.CreateEmptyDiskImage(c, 20)

	c.Check(InitializeLUKS2Container(path, "data", key, nil), IsNil)

	newKey := s.newPrimaryKey()
	c.Check(AddLUKS2ContainerUnlockKey(path, data.keyslotName, key, newKey, data.options), IsNil)

	options := data.options
	if options == nil {
		options = &KDFOptions{MemoryKiB: 32, ForceIterations: 4}
	}

	expectedName := "default"
	if data.keyslotName != "" {
		expectedName = data.keyslotName
	}

	info, err := luks2.ReadHeader(path, luks2.LockModeBlocking)
	c.Assert(err, IsNil)

	c.Check(info.Metadata.Keyslots, HasLen, 2)
	keyslot, ok := info.Metadata.Keyslots[1]
	c.Assert(ok, Equals, true)
	c.Check(keyslot.Priority, Equals, luks2.SlotPriorityHigh)

	c.Check(info.Metadata.Tokens, HasLen, 2)
	c.Check(info.Metadata.Tokens[1], DeepEquals, luks2.Token(&luksview.KeyDataToken{
		TokenBase: luksview.TokenBase{
			TokenKeyslot: 1,
			TokenName:    expectedName},
		Priority: 0}))

	expectedMemoryKiB := 1 * 1024 * 1024
	if options.MemoryKiB > 0 {
		expectedMemoryKiB = options.MemoryKiB
	}

	if options.ForceIterations > 0 {
		c.Check(keyslot.KDF.Time, Equals, options.ForceIterations)
		c.Check(keyslot.KDF.Memory, Equals, expectedMemoryKiB)
		luks2test.CheckLUKS2Passphrase(c, path, newKey)
	} else {
		expectedKDFTime := 2 * time.Second
		if options.TargetDuration > 0 {
			expectedKDFTime = options.TargetDuration
		}

		c.Check(keyslot.KDF.Memory, snapd_testutil.IntLessEqual, expectedMemoryKiB)

		start := time.Now()
		luks2test.CheckLUKS2Passphrase(c, path, newKey)
		elapsed := time.Now().Sub(start)

		// Check KDF time here with +/-20% tolerance and additional 500ms for cryptsetup exec and other activities
		c.Check(int(elapsed/time.Millisecond), snapd_testutil.IntGreaterThan, int(float64(expectedKDFTime/time.Millisecond)*0.8))
		c.Check(int(elapsed/time.Millisecond), snapd_testutil.IntLessThan, int(float64(expectedKDFTime/time.Millisecond)*1.2)+500)
	}

	luks2test.CheckLUKS2Passphrase(c, path, key)
}

func (s *cryptSuiteUnmocked) TestAddLUKS2ContainerUnlockKey(c *C) {
	s.testAddLUKS2ContainerUnlockKey(c, &testAddLUKS2ContainerUnlockKeyUnmockedData{
		keyslotName: "foo",
	})
}

func (s *cryptSuiteUnmocked) TestAddLUKS2ContainerUnlockKeyDifferentName(c *C) {
	s.testAddLUKS2ContainerUnlockKey(c, &testAddLUKS2ContainerUnlockKeyUnmockedData{
		keyslotName: "bar",
	})
}

func (s *cryptSuiteUnmockedExpensive) TestAddLUKS2ContainerUnlockKeyWithCustomKDFTime(c *C) {
	s.testAddLUKS2ContainerUnlockKey(c, &testAddLUKS2ContainerUnlockKeyUnmockedData{
		keyslotName: "foo",
		options:     &KDFOptions{TargetDuration: 100 * time.Millisecond},
	})
}

func (s *cryptSuiteUnmockedExpensive) TestAddLUKS2ContainerUnlockKeyWithCustomKDFMemory(c *C) {
	s.testAddLUKS2ContainerUnlockKey(c, &testAddLUKS2ContainerUnlockKeyUnmockedData{
		keyslotName: "foo",
		options:     &KDFOptions{MemoryKiB: 64},
	})
}

func (s *cryptSuiteUnmocked) TestAddLUKS2ContainerUnlockKeyWithCustomKDFIterations(c *C) {
	s.testAddLUKS2ContainerUnlockKey(c, &testAddLUKS2ContainerUnlockKeyUnmockedData{
		keyslotName: "foo",
		options:     &KDFOptions{MemoryKiB: 32, ForceIterations: 8},
	})
}

type testAddLUKS2ContainerRecoveryKeyUnmockedData struct {
	keyslotName string
	options     *KDFOptions
}

func (s *cryptSuiteUnmockedBase) testAddLUKS2ContainerRecoveryKey(c *C, data *testAddLUKS2ContainerRecoveryKeyUnmockedData) {
	key := s.newPrimaryKey()
	path := luks2test.CreateEmptyDiskImage(c, 20)

	c.Check(InitializeLUKS2Container(path, "data", key, nil), IsNil)

	var recoveryKey RecoveryKey
	rand.Read(recoveryKey[:])
	c.Check(AddLUKS2ContainerRecoveryKey(path, data.keyslotName, key, recoveryKey, data.options), IsNil)

	options := data.options
	if options == nil {
		options = &KDFOptions{}
	}

	expectedName := "default-recovery"
	if data.keyslotName != "" {
		expectedName = data.keyslotName
	}

	info, err := luks2.ReadHeader(path, luks2.LockModeBlocking)
	c.Assert(err, IsNil)

	c.Check(info.Metadata.Keyslots, HasLen, 2)
	keyslot, ok := info.Metadata.Keyslots[1]
	c.Assert(ok, Equals, true)
	c.Check(keyslot.Priority, Equals, luks2.SlotPriorityNormal)

	c.Check(info.Metadata.Tokens, HasLen, 2)
	c.Check(info.Metadata.Tokens[1], DeepEquals, luks2.Token(&luksview.RecoveryToken{
		TokenBase: luksview.TokenBase{
			TokenKeyslot: 1,
			TokenName:    expectedName}}))

	expectedMemoryKiB := 1 * 1024 * 1024
	if options.MemoryKiB > 0 {
		expectedMemoryKiB = options.MemoryKiB
	}

	if options.ForceIterations > 0 {
		c.Check(keyslot.KDF.Time, Equals, options.ForceIterations)
		c.Check(keyslot.KDF.Memory, Equals, expectedMemoryKiB)
		luks2test.CheckLUKS2Passphrase(c, path, recoveryKey[:])
	} else {
		expectedKDFTime := 2 * time.Second
		if options.TargetDuration > 0 {
			expectedKDFTime = options.TargetDuration
		}

		c.Check(keyslot.KDF.Memory, snapd_testutil.IntLessEqual, expectedMemoryKiB)

		start := time.Now()
		luks2test.CheckLUKS2Passphrase(c, path, recoveryKey[:])
		elapsed := time.Now().Sub(start)

		// Check KDF time here with +/-20% tolerance and additional 500ms for cryptsetup exec and other activities
		c.Check(int(elapsed/time.Millisecond), snapd_testutil.IntGreaterThan, int(float64(expectedKDFTime/time.Millisecond)*0.8))
		c.Check(int(elapsed/time.Millisecond), snapd_testutil.IntLessThan, int(float64(expectedKDFTime/time.Millisecond)*1.2)+500)
	}

	luks2test.CheckLUKS2Passphrase(c, path, key)
}

func (s *cryptSuiteUnmockedExpensive) TestAddLUKS2ContainerRecoveryKey(c *C) {
	s.testAddLUKS2ContainerRecoveryKey(c, &testAddLUKS2ContainerRecoveryKeyUnmockedData{})
}

func (s *cryptSuiteUnmockedExpensive) TestAddLUKS2ContainerRecoveryKeyDifferentName(c *C) {
	s.testAddLUKS2ContainerRecoveryKey(c, &testAddLUKS2ContainerRecoveryKeyUnmockedData{
		keyslotName: "foo",
	})
}

func (s *cryptSuiteUnmockedExpensive) TestAddLUKS2ContainerRecoveryKeyWithCustomKDFTime(c *C) {
	s.testAddLUKS2ContainerRecoveryKey(c, &testAddLUKS2ContainerRecoveryKeyUnmockedData{
		options: &KDFOptions{TargetDuration: 100 * time.Millisecond},
	})
}

func (s *cryptSuiteUnmockedExpensive) TestAddLUKS2ContainerRecoveryKeyWithCustomKDFMemory(c *C) {
	s.testAddLUKS2ContainerRecoveryKey(c, &testAddLUKS2ContainerRecoveryKeyUnmockedData{
		options: &KDFOptions{MemoryKiB: 64},
	})
}

func (s *cryptSuiteUnmocked) TestAddLUKS2ContainerRecoveryKeyWithCustomKDFIterations(c *C) {
	s.testAddLUKS2ContainerRecoveryKey(c, &testAddLUKS2ContainerRecoveryKeyUnmockedData{
		options: &KDFOptions{MemoryKiB: 32, ForceIterations: 8},
	})
}

func (s *cryptSuiteUnmockedExpensive) TestListLUKS2ContainerKeyName(c *C) {
	key := s.newPrimaryKey()
	path := luks2test.CreateEmptyDiskImage(c, 20)

	c.Check(InitializeLUKS2Container(path, "data", key, nil), IsNil)
	c.Check(AddLUKS2ContainerUnlockKey(path, "bar", key, key, nil), IsNil)

	var recoveryKey RecoveryKey
	rand.Read(recoveryKey[:])
	c.Check(AddLUKS2ContainerRecoveryKey(path, "", key, recoveryKey, nil), IsNil)

	names, err := ListLUKS2ContainerUnlockKeyNames(path)
	c.Check(err, IsNil)
	c.Check(names, DeepEquals, []string{"bar", "default"})

	names, err = ListLUKS2ContainerRecoveryKeyNames(path)
	c.Check(err, IsNil)
	c.Check(names, DeepEquals, []string{"default-recovery"})
}

type testDeleteLUKS2ContainerKeyUnmockedData struct {
	key                   DiskUnlockKey
	recoveryKey           RecoveryKey
	name                  string
	existingKey           DiskUnlockKey
	expectedUnlockNames   []string
	expectedRecoveryNames []string
	expectedToken         luks2.Token
}

func (s *cryptSuiteUnmocked) testDeleteLUKS2ContainerKey(c *C, data *testDeleteLUKS2ContainerKeyUnmockedData) {
	path := luks2test.CreateEmptyDiskImage(c, 20)

	c.Check(InitializeLUKS2Container(path, "data", data.key, nil), IsNil)

	var recoveryKey RecoveryKey
	rand.Read(recoveryKey[:])
	c.Check(AddLUKS2ContainerRecoveryKey(path, "", data.key, data.recoveryKey, &KDFOptions{MemoryKiB: 32, ForceIterations: 4}), IsNil)

	c.Check(DeleteLUKS2ContainerKey(path, data.name, data.existingKey), IsNil)

	names, err := ListLUKS2ContainerUnlockKeyNames(path)
	c.Check(err, IsNil)
	c.Check(names, DeepEquals, data.expectedUnlockNames)

	names, err = ListLUKS2ContainerRecoveryKeyNames(path)
	c.Check(err, IsNil)
	c.Check(names, DeepEquals, data.expectedRecoveryNames)

	info, err := luks2.ReadHeader(path, luks2.LockModeBlocking)
	c.Assert(err, IsNil)

	c.Check(info.Metadata.Tokens, HasLen, 1)

	var usedSlot int
	for _, token := range info.Metadata.Tokens {
		c.Check(token, DeepEquals, data.expectedToken)
		usedSlot = token.Keyslots()[0]
		break
	}

	c.Check(info.Metadata.Keyslots, HasLen, 1)
	_, ok := info.Metadata.Keyslots[usedSlot]
	c.Check(ok, testutil.IsTrue)
}

func (s *cryptSuiteUnmocked) TestDeleteLUKS2ContainerKey1(c *C) {
	key := s.newPrimaryKey()
	recoveryKey := s.newRecoveryKey()

	s.testDeleteLUKS2ContainerKey(c, &testDeleteLUKS2ContainerKeyUnmockedData{
		key:                   key,
		recoveryKey:           recoveryKey,
		name:                  "default",
		existingKey:           DiskUnlockKey(recoveryKey[:]),
		expectedRecoveryNames: []string{"default-recovery"},
		expectedToken: &luksview.RecoveryToken{
			TokenBase: luksview.TokenBase{
				TokenName:    "default-recovery",
				TokenKeyslot: 1}}})
}

func (s *cryptSuiteUnmocked) TestDeleteLUKS2ContainerKey2(c *C) {
	key := s.newPrimaryKey()
	recoveryKey := s.newRecoveryKey()

	s.testDeleteLUKS2ContainerKey(c, &testDeleteLUKS2ContainerKeyUnmockedData{
		key:                 key,
		recoveryKey:         recoveryKey,
		name:                "default-recovery",
		existingKey:         key,
		expectedUnlockNames: []string{"default"},
		expectedToken: &luksview.KeyDataToken{
			TokenBase: luksview.TokenBase{
				TokenName:    "default",
				TokenKeyslot: 0}}})
}

type testRenameLUKS2ContainerKeyUnmockedData struct {
	old                  string
	new                  string
	expectedUnlockName   string
	expectedRecoveryName string
	expectedToken        luks2.Token
}

func (s *cryptSuiteUnmocked) testRenameLUKS2ContainerKey(c *C, data *testRenameLUKS2ContainerKeyUnmockedData) {
	if luks2.DetectCryptsetupFeatures()&luks2.FeatureTokenReplace == 0 {
		c.Skip("cryptsetup doesn't support token replace")
	}

	key := s.newPrimaryKey()
	path := luks2test.CreateEmptyDiskImage(c, 20)

	c.Check(InitializeLUKS2Container(path, "data", key, nil), IsNil)

	var recoveryKey RecoveryKey
	rand.Read(recoveryKey[:])
	c.Check(AddLUKS2ContainerRecoveryKey(path, "", key, recoveryKey, &KDFOptions{MemoryKiB: 32, ForceIterations: 4}), IsNil)

	c.Check(RenameLUKS2ContainerKey(path, data.old, data.new), IsNil)

	names, err := ListLUKS2ContainerUnlockKeyNames(path)
	c.Check(err, IsNil)
	c.Check(names, DeepEquals, []string{data.expectedUnlockName})

	names, err = ListLUKS2ContainerRecoveryKeyNames(path)
	c.Check(err, IsNil)
	c.Check(names, DeepEquals, []string{data.expectedRecoveryName})

	info, err := luks2.ReadHeader(path, luks2.LockModeBlocking)
	c.Assert(err, IsNil)

	found := false
	for _, token := range info.Metadata.Tokens {
		named, ok := token.(luksview.NamedToken)
		if !ok {
			continue
		}

		c.Check(named.Name(), Not(Equals), data.old)

		if named.Name() == data.new {
			c.Check(found, Not(testutil.IsTrue))
			c.Check(token, DeepEquals, data.expectedToken)
			found = true
		}
	}
	c.Check(found, testutil.IsTrue)
}

func (s *cryptSuiteUnmocked) TestRenameLUKS2ContainerUnlockKey(c *C) {
	s.testRenameLUKS2ContainerKey(c, &testRenameLUKS2ContainerKeyUnmockedData{
		old:                  "default",
		new:                  "foo",
		expectedUnlockName:   "foo",
		expectedRecoveryName: "default-recovery",
		expectedToken: &luksview.KeyDataToken{
			TokenBase: luksview.TokenBase{
				TokenName:    "foo",
				TokenKeyslot: 0}}})
}

func (s *cryptSuiteUnmocked) TestRenameLUKS2ContainerRecoveryKey(c *C) {
	s.testRenameLUKS2ContainerKey(c, &testRenameLUKS2ContainerKeyUnmockedData{
		old:                  "default-recovery",
		new:                  "bar",
		expectedUnlockName:   "default",
		expectedRecoveryName: "bar",
		expectedToken: &luksview.RecoveryToken{
			TokenBase: luksview.TokenBase{
				TokenName:    "bar",
				TokenKeyslot: 1}}})
}
