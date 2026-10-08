package sshprovisioner

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/provisioner"
)

const (
	dataStorageContractVersion = "HOR-545/v2"
	dataStorageReceiptPath     = "/var/lib/iterabase/data-storage.receipt"
	lvmReportPairParser        = `awk -F'|' '{for(i=1;i<=2;i++){gsub(/^[[:space:]]+|[[:space:]]+$/, "", $i)}; print $1 "|" $2}'`
)

// dataStorageDeviceResolutionPrelude is the shared read-only device identity
// prelude used by BOTH reconcile and purge so a single lifecycle path cannot
// silently accept state the other rejects. It resolves the stable by-id set,
// captures model/serial/WWN/size/transport, and rejects non-whole/logical
// disks, duplicates, or missing identities. It performs no mutation and emits
// no result lines; callers layer reconcile/purge transactions on top. The
// returned string uses single '%' characters and is injected through an outer
// fmt.Sprintf %s argument, never parsed as part of a format verb.
func dataStorageDeviceResolutionPrelude() string {
	return `
sanitize() { printf '%s' "$1" | tr '\t|\r\n' '    '; }
resolved=(); model=(); serial=(); wwn=(); size=(); transport=(); kname=(); planned_pv_uuid=()
count=${#selected[@]}
test "$count" -gt 0 || fail "selected device set is empty"
# FORGE_DATA_STORAGE_FIXTURE_LOOP is an INTERNAL test-process-only flag. It is
# never set by Forge apply, the Forge CLI, or any configuration file (it is not a
# supported knob), and it is injected solely by the privileged forge fault-matrix
# CI harness when it executes the real script over loop-backed by-id fixtures. In
# any other invocation the ordinary production rejection of logical/loop devices
# and unsigned disks is retained unchanged.
for ((i=0; i<count; i++)); do
  path=${selected[$i]}
  case "$path" in /dev/disk/by-id/*) ;; *) fail "selected device $path is not a stable /dev/disk/by-id identity" ;; esac
  case "$path" in *-part[0-9]*) fail "selected device $path is a partition identity" ;; esac
  test -L "$path" || fail "selected stable identity $path is missing or not a symlink"
  dev=$(readlink -f -- "$path")
  test -b "$dev" || fail "selected identity $path does not resolve to a block device"
  if [ "${FORGE_DATA_STORAGE_FIXTURE_LOOP:-}" != "1" ]; then
    test "$(lsblk -dnro TYPE -- "$dev")" = disk || fail "$path is not a whole disk"
  fi
  test "$(lsblk -dnro RM -- "$dev")" = 0 || fail "$path is removable"
  resolved[$i]=$dev
  kname[$i]=$(lsblk -dnro KNAME -- "$dev")
  test -n "${kname[$i]}" || fail "cannot determine kernel identity for $path"
  if [ "${FORGE_DATA_STORAGE_FIXTURE_LOOP:-}" != "1" ]; then
    case "${kname[$i]}" in loop*|dm-*|md*|zd*|nbd*) fail "selected device $path is an unsupported logical/network device" ;; esac
  fi
  model[$i]=$(sanitize "$(lsblk -dnro MODEL -- "$dev")")
  serial[$i]=$(sanitize "$(lsblk -dnro SERIAL -- "$dev")")
  wwn[$i]=$(sanitize "$(lsblk -dnro WWN -- "$dev")")
  size[$i]=$(lsblk -bdnro SIZE -- "$dev")
  case "${size[$i]}" in ''|*[!0-9]*) fail "selected disk $path size probe is invalid" ;; esac
  if [ "${FORGE_DATA_STORAGE_FIXTURE_LOOP:-}" != "1" ]; then
    test -n "${serial[$i]}${wwn[$i]}" || fail "selected disk $path exposes neither serial nor WWN"
  fi
  transport[$i]=$(lsblk -dnro TRAN -- "$dev" | tr '[:upper:]' '[:lower:]' | tr '\t\r\n' '   ' | awk '{$1=$1; print}')
  transport[$i]=${transport[$i]:-unknown}
  for ((j=0; j<i; j++)); do
    test "${resolved[$j]}" != "$dev" || fail "selected stable identities ${selected[$j]} and $path resolve to the same disk"
  done
  planned_pv_uuid[$i]=
done
`
}

// ListDataStorageDevices lists blank stable whole-disk candidates without
// reading arbitrary device bytes or mutating the host.
func (p *SSHProvisioner) ListDataStorageDevices(ctx context.Context) ([]provisioner.DataStorageDevice, error) {
	cmd := `sudo bash -ceu '
for tool in readlink lsblk wipefs sort tr awk; do command -v "$tool" >/dev/null; done
for selected in /dev/disk/by-id/*; do
  test -L "$selected" || continue
  case "$selected" in *-part[0-9]*) continue ;; esac
  device=$(readlink -f -- "$selected")
  test -b "$device" || continue
  test "$(lsblk -dnro TYPE -- "$device")" = disk || continue
  test "$(lsblk -dnro RM -- "$device")" = 0 || continue
  test "$(lsblk -nrpo PATH -- "$device" | awk "NF {n++} END {print n+0}")" = 1 || continue
  test -z "$(lsblk -dnro PTTYPE -- "$device")" || continue
  signatures=$(wipefs -n --noheadings --output TYPE -- "$device") || exit 42
  test -z "$(printf "%s" "$signatures" | awk "NF")" || continue
  model=$(lsblk -dnro MODEL -- "$device" | tr "\t\r\n" "   ")
  serial=$(lsblk -dnro SERIAL -- "$device" | tr "\t\r\n" "   ")
  size=$(lsblk -bdnro SIZE -- "$device")
  transport=$(lsblk -dnro TRAN -- "$device" | tr "[:upper:]" "[:lower:]" | tr "\t\r\n" "   ")
  printf "FORGE_DATA_STORAGE_DEVICE\t%s\t%s\t%s\t%s\t%s\n" "$selected" "$model" "$serial" "$size" "$transport"
done | sort -u
'`
	out, err := p.run(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("list stable blank data-storage devices: %w", err)
	}
	var devices []provisioner.DataStorageDevice
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 6 || parts[0] != "FORGE_DATA_STORAGE_DEVICE" {
			continue
		}
		size, parseErr := strconv.ParseUint(strings.TrimSpace(parts[4]), 10, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("parse data-storage device size from %q: %w", line, parseErr)
		}
		devices = append(devices, provisioner.DataStorageDevice{
			Path: parts[1], Model: strings.TrimSpace(parts[2]), Serial: strings.TrimSpace(parts[3]),
			SizeBytes: size, Transport: strings.TrimSpace(parts[5]),
		})
	}
	return devices, nil
}

// EnsureDataStorageTools installs/verifies the host-owned LVM/XFS tools and
// converges the superseded pre-release dm-snapshot configuration to absence
// without touching selected disks.
func (p *SSHProvisioner) EnsureDataStorageTools(ctx context.Context) error {
	const verify = "command -v pvcreate >/dev/null && command -v pvremove >/dev/null && command -v vgcreate >/dev/null && command -v vgremove >/dev/null && command -v pvs >/dev/null && command -v vgs >/dev/null && command -v lvs >/dev/null && command -v mkfs.xfs >/dev/null && command -v xfs_info >/dev/null && command -v fuser >/dev/null"
	if _, err := p.run(ctx, verify); err != nil {
		cmd := "sudo apt-get update && sudo apt-get install -y lvm2 xfsprogs psmisc"
		for attempt := 0; ; attempt++ {
			out, installErr := p.run(ctx, cmd)
			if installErr == nil {
				break
			}
			if (!isAptLockHeld(installErr.Error()) && !isAptLockHeld(out)) || attempt >= 20 {
				return fmt.Errorf("install required data-storage tooling (lvm2 xfsprogs psmisc): %w", installErr)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(aptLockRetryInterval):
			}
		}
	}
	if _, err := p.run(ctx, verify); err != nil {
		return fmt.Errorf("verify required data-storage tooling: %w", err)
	}
	disableSnapshotModule := `sudo bash -ceu '
config=/etc/modules-load.d/iterabase-data.conf
if test -e "$config"; then
  test -f "$config" && test ! -L "$config"
  test "$(cat "$config")" = dm-snapshot
fi
if grep -q "^dm_snapshot " /proc/modules; then
  command -v modprobe >/dev/null
  modprobe -r dm-snapshot
fi
if test -e "$config"; then
  rm -f -- "$config"
  sync -f /etc/modules-load.d
fi
test ! -e "$config"
! grep -q "^dm_snapshot " /proc/modules
'`
	if _, err := p.run(ctx, disableSnapshotModule); err != nil {
		return fmt.Errorf("converge unsupported dm-snapshot module/configuration to absence: %w", err)
	}
	return nil
}

func (p *SSHProvisioner) InspectDataStorage(ctx context.Context, spec provisioner.DataStorageSpec) (*provisioner.DataStorageState, error) {
	return p.runDataStorage(ctx, spec, false)
}

func (p *SSHProvisioner) ReconcileDataStorage(ctx context.Context, spec provisioner.DataStorageSpec) (*provisioner.DataStorageState, error) {
	return p.runDataStorage(ctx, spec, true)
}

func (p *SSHProvisioner) runDataStorage(ctx context.Context, spec provisioner.DataStorageSpec, reconcile bool) (*provisioner.DataStorageState, error) {
	mode := "inspect"
	if reconcile {
		mode = "reconcile"
	}
	out, err := p.run(ctx, "sudo bash -ceu "+shellQuote(dataStorageReconcileScript(spec, mode)))
	if err != nil {
		return nil, fmt.Errorf("%s data storage %v: %w", mode, spec.Devices, err)
	}
	state, err := parseDataStorageResult(out)
	if err != nil {
		return nil, err
	}
	return state, nil
}

func parseDataStorageResult(out string) (*provisioner.DataStorageState, error) {
	state := &provisioner.DataStorageState{}
	declaredDevices := -1
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		switch {
		case len(parts) == 9 && parts[0] == "FORGE_DATA_STORAGE_DEVICE_RESULT":
			size, err := strconv.ParseUint(parts[7], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse data-storage device result size: %w", err)
			}
			state.Devices = append(state.Devices, provisioner.DataStorageDevice{
				Path: parts[1], Resolved: parts[2], Model: parts[3], Serial: parts[4], WWN: parts[5],
				PVUUID: parts[6], SizeBytes: size, Transport: parts[8],
			})
		case len(parts) == 7 && parts[0] == "FORGE_DATA_STORAGE_RESULT":
			size, err := strconv.ParseUint(parts[4], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse data-storage VG size: %w", err)
			}
			free, err := strconv.ParseUint(parts[5], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse data-storage VG free size: %w", err)
			}
			declaredDevices, err = strconv.Atoi(parts[6])
			if err != nil {
				return nil, fmt.Errorf("parse data-storage device count: %w", err)
			}
			state.State, state.VGName, state.VGUUID, state.SizeBytes, state.FreeBytes = parts[1], parts[2], parts[3], size, free
		}
	}
	if declaredDevices < 0 {
		return nil, fmt.Errorf("data-storage reconciliation returned no bounded result")
	}
	if declaredDevices != len(state.Devices) {
		return nil, fmt.Errorf("data-storage reconciliation returned %d device records, expected %d", len(state.Devices), declaredDevices)
	}
	return state, nil
}

func dataStorageReconcileScript(spec provisioner.DataStorageSpec, mode string) string {
	devices := append([]string(nil), spec.Devices...)
	sort.Strings(devices)
	quoted := make([]string, len(devices))
	for i, device := range devices {
		quoted[i] = shellQuote(device)
	}
	return fmt.Sprintf(`
install_name=%s
mode=%s
contract=%s
receipt=%s
vg_name=%s
selected=(%s)

fail() { printf 'data-storage refusal: %%s\n' "$*" >&2; exit 42; }
need() { command -v "$1" >/dev/null 2>&1 || fail "required probe/tool $1 is unavailable"; }
for tool in readlink lsblk findmnt blkid wipefs awk grep stat base64 sync find tr head sort dirname mktemp mv chown chmod cat seq sleep; do need "$tool"; done
%s

list_process_ids() {
  local process_list process_list_rc process_list_error process_list_attempt
  process_list_error=
  for process_list_attempt in 1 2 3; do
    set +e
    process_list=$(set -o pipefail; LC_ALL=C ls -1U -- /proc 2>&1 | head -n 65537)
    process_list_rc=$?
    set -e
    if test "$process_list_rc" = 0; then
      test -n "$process_list" || fail "active-open probe found an empty /proc process table"
      test "$(printf '%%s\n' "$process_list" | awk 'END {print NR+0}')" -le 65536 || fail "active-open probe exceeded its bounded /proc-entry limit"
      printf '%%s\n' "$process_list" | awk '/^[0-9]+$/'
      return 0
    fi
    process_list_error=$process_list
  done
  fail "active-open probe could not enumerate /proc: $process_list_error"
}

list_process_fds() {
  local process_path fd_list_rc fd_list fd_list_error fd_list_attempt process_id current_process_ids
  process_path=$1
  fd_list_error=
  for fd_list_attempt in 1 2 3; do
    set +e
    fd_list=$(set -o pipefail; LC_ALL=C ls -1U -- "$process_path/fd" 2>&1 | head -n 65537)
    fd_list_rc=$?
    set -e
    if test "$fd_list_rc" = 0; then printf '%%s' "$fd_list"; return 0; fi
    fd_list_error=$fd_list
    current_process_ids=$(list_process_ids)
    process_id=${process_path##*/}
    if ! printf '%%s\n' "$current_process_ids" | grep -Fxq "$process_id"; then return 0; fi
  done
  fail "active-open probe could not enumerate $process_path/fd: $fd_list_error"
}

probe_active_raw_consumer() {
  local dev device_number scanned_fds process_ids process fd_names fd_name fd fd_rc fd_number fd_gone fd_round fd_attempt remaining_fds
  dev=$1
  device_number=$(stat -Lc '%%t:%%T' "$dev" 2>/dev/null) || fail "cannot determine $dev device number"
  scanned_fds=0
  process_ids=$(list_process_ids)
  test -n "$process_ids" || fail "active-open probe found no visible processes"
  while IFS= read -r process_id; do
    process=/proc/$process_id
    fd_names=$(list_process_fds "$process")
    test -n "$fd_names" || continue
    while IFS= read -r fd_name; do
      case "$fd_name" in ''|*[!0-9]*) fail "active-open probe found invalid descriptor $process/fd/$fd_name" ;; esac
      scanned_fds=$((scanned_fds + 1)); test "$scanned_fds" -le 65536 || fail "active-open probe exceeded its descriptor bound"
      fd=$process/fd/$fd_name; fd_rc=1; fd_number=; fd_gone=false
      for fd_round in 1 2 3; do
        for fd_attempt in 1 2 3; do
          set +e; fd_number=$(stat -Lc '%%t:%%T' "$fd" 2>&1); fd_rc=$?; set -e
          test "$fd_rc" = 0 && break
        done
        test "$fd_rc" = 0 && break
        remaining_fds=$(list_process_fds "$process")
        if ! printf '%%s\n' "$remaining_fds" | grep -Fxq "$fd_name"; then fd_gone=true; break; fi
      done
      test "$fd_gone" = false || continue
      test "$fd_rc" = 0 || fail "active-open probe could not inspect $fd: $fd_number"
      test "$fd_number" != "$device_number" || fail "$dev is held open as a raw block device by process $process_id"
    done <<EOF
$fd_names
EOF
  done <<EOF
$process_ids
EOF
}

probe_identity_topology() {
  local i=$1 path dev kernel source direct_mounts
  path=${selected[$i]}; dev=${resolved[$i]}; kernel=${kname[$i]}
  test -L "$path" && test "$(readlink -f -- "$path")" = "$dev" || fail "$path identity drifted"
  if [ "${FORGE_DATA_STORAGE_FIXTURE_LOOP:-}" != "1" ]; then
    test "$(lsblk -dnro TYPE -- "$dev")" = disk || fail "$path is not a whole disk"
  fi
  test "$(lsblk -dnro RM -- "$dev")" = 0 || fail "$path is removable"
  test -z "$(lsblk -dnro PTTYPE -- "$dev")" || fail "$path has a partition table"
  test "$(sanitize "$(lsblk -dnro MODEL -- "$dev")")" = "${model[$i]}" || fail "$path model identity drifted"
  test "$(sanitize "$(lsblk -dnro SERIAL -- "$dev")")" = "${serial[$i]}" || fail "$path serial identity drifted"
  test "$(sanitize "$(lsblk -dnro WWN -- "$dev")")" = "${wwn[$i]}" || fail "$path WWN identity drifted"
  test "$(lsblk -bdnro SIZE -- "$dev")" = "${size[$i]}" || fail "$path size identity drifted"
  for target in / /boot /boot/efi /var /var/lib/rancher/k3s /var/lib/kubelet; do
    source=$(findmnt -n -o SOURCE --target "$target" 2>/dev/null || true); source=${source%%[*}
    test -n "$source" || continue; source=$(readlink -f -- "$source" 2>/dev/null || true); test -b "$source" || continue
    if lsblk -snro PATH -- "$source" | grep -Fxq "$dev"; then fail "$path backs system path $target"; fi
  done
  while read -r source _; do
    test "$source" != Filename || continue; source=$(readlink -f -- "$source" 2>/dev/null || true); test -b "$source" || continue
    if lsblk -snro PATH -- "$source" | grep -Fxq "$dev"; then fail "$path backs active swap"; fi
  done < /proc/swaps
  direct_mounts=$(findmnt -rn -S "$dev" -o TARGET 2>/dev/null || true)
  test -z "$direct_mounts" || fail "$path is directly mounted at $direct_mounts"
  probe_active_raw_consumer "$dev"
}

probe_no_child_or_holder() {
  local i=$1 dev kernel
  dev=${resolved[$i]}; kernel=${kname[$i]}
  test "$(lsblk -nrpo PATH -- "$dev" | awk 'NF {n++} END {print n+0}')" = 1 || fail "${selected[$i]} has partitions or child devices"
  test ! -d "/sys/class/block/$kernel/holders" || test -z "$(find "/sys/class/block/$kernel/holders" -mindepth 1 -maxdepth 1 -print -quit)" || fail "${selected[$i]} has active holders"
}

probe_blank() {
  local i=$1 dev wipe_types wipe_rc blk_type blk_rc
  probe_identity_topology "$i"; dev=${resolved[$i]}
  probe_no_child_or_holder "$i"
  set +e; wipe_types=$(wipefs -n --noheadings --output TYPE -- "$dev" 2>&1); wipe_rc=$?; set -e
  test "$wipe_rc" = 0 || fail "wipefs probe failed for ${selected[$i]}: $wipe_types"
  test -z "$(printf '%%s' "$wipe_types" | awk 'NF')" || fail "${selected[$i]} has a recognized partition/filesystem/RAID/LVM/crypt signature"
  set +e; blk_type=$(blkid -p -s TYPE -o value -- "$dev" 2>&1); blk_rc=$?; set -e
  test "$blk_rc" = 2 || { test "$blk_rc" = 0 && fail "${selected[$i]} has recognized signature $blk_type"; fail "blkid probe failed or was ambiguous for ${selected[$i]}: $blk_type"; }
}

lvm_uuid() { local raw; raw=$(tr -d - < /proc/sys/kernel/random/uuid); printf '%%s-%%s-%%s-%%s-%%s-%%s-%%s\n' "${raw:0:6}" "${raw:6:4}" "${raw:10:4}" "${raw:14:4}" "${raw:18:4}" "${raw:22:4}" "${raw:26:6}"; }
new_ownership_tag() {
  local raw
  raw=$(tr -d - < /proc/sys/kernel/random/uuid)
  printf '%%s' "$raw" | grep -Eq '^[0-9a-f]{32}$' || fail "kernel UUID source did not produce a valid ownership token"
  printf 'iterabase.hor545.%%s\n' "$raw"
}
ownership_tag_owners() {
  vgs --noheadings --separator '|' -o vg_name,vg_uuid,vg_tags | awk -F'|' -v wanted="$ownership_tag" '
    {for(i=1;i<=3;i++) gsub(/^[[:space:]]+|[[:space:]]+$/, "", $i); n=split($3, tags, ","); for(i=1;i<=n;i++) if(tags[i] == wanted) {print $1 "|" $2; break}}
  ' | sort
}
%s
%s
%s
if test "$receipt_present" = 1 && test "$mode" = reconcile; then
  case "$status" in purge-*) fail "data-storage purge is in progress; resume explicit destroy --purge-data-storage before apply" ;; esac
  if test "$status" = complete; then storage_stage_barrier reapply-inspected-complete; fi
fi
if test "$receipt_present" = 0; then
  for ((i=0; i<count; i++)); do probe_blank "$i"; done
  if command -v vgs >/dev/null 2>&1 && vgs "$vg_name" >/dev/null 2>&1; then fail "fixed VG $vg_name already exists without the Forge receipt"; fi
  if test "$mode" = inspect; then
    for ((i=0; i<count; i++)); do printf 'FORGE_DATA_STORAGE_DEVICE_RESULT\t%%s\t%%s\t%%s\t%%s\t%%s\t\t%%s\t%%s\n' "${selected[$i]}" "${resolved[$i]}" "${model[$i]}" "${serial[$i]}" "${wwn[$i]}" "${size[$i]}" "${transport[$i]}"; done
    printf 'FORGE_DATA_STORAGE_RESULT\tblank-candidate\t%%s\t\t0\t0\t%%s\n' "$vg_name" "$count"; exit 0
  fi
  need pvcreate; need pvs; need vgcreate; need vgs; need lvs
  for ((i=0; i<count; i++)); do planned_pv_uuid[$i]=$(lvm_uuid); done
  ownership_tag=$(new_ownership_tag)
  test -z "$(ownership_tag_owners)" || fail "generated data-storage ownership tag already belongs to a VG"
  purge_done=0; write_receipt planned 0 0; status=planned; storage_stage_barrier receipt-planned
fi

need pvs; need vgs; need lvs
tag_owners=$(ownership_tag_owners)
if test -n "$tag_owners"; then
  test "$(printf '%%s\n' "$tag_owners" | awk 'NF {n++} END {print n+0}')" = 1 || fail "data-storage ownership tag belongs to multiple VGs"
  test "${tag_owners%%|*}" = "$vg_name" || fail "data-storage ownership tag belongs to foreign VG ${tag_owners%%|*}"
fi
read_pv() {
  local pv_line pv_rc
  set +e; pv_line=$(pvs --noheadings --separator '|' -o pv_uuid,vg_name -- "$1" 2>/dev/null); pv_rc=$?; set -e
  test "$pv_rc" = 0 || return 1
  printf '%%s' "$pv_line" | `+lvmReportPairParser+`
}
verify_or_blank_set() {
  local q pv pv_uuid pv_vg
  for ((q=0; q<count; q++)); do
    probe_identity_topology "$q"
    if pv=$(read_pv "${resolved[$q]}"); then
      pv_uuid=${pv%%|*}; pv_vg=${pv#*|}
      test "$pv_uuid" = "${planned_pv_uuid[$q]}" || fail "${selected[$q]} PV UUID is foreign"
      test -z "$pv_vg" || test "$pv_vg" = "$vg_name" || fail "${selected[$q]} belongs to foreign VG $pv_vg"
      # Child/holder drift on an already-created PV is only meaningful during
      # CREATE/concile (before any LV exists): a receipt VG that legitimately owns
      # LVs will always have child devices at reapply/inspect time, so the
      # zero-child probe must not run there. Probe on the mutation path only.
      test "$mode" = inspect || probe_no_child_or_holder "$q"
    else
      probe_blank "$q"
    fi
  done
}

if test "$mode" = inspect; then
  verify_or_blank_set
else
  for ((i=0; i<count; i++)); do
    if pv=$(read_pv "${resolved[$i]}"); then
      pv_uuid=${pv%%|*}; pv_vg=${pv#*|}
      test "$pv_uuid" = "${planned_pv_uuid[$i]}" || fail "${selected[$i]} PV UUID differs from the planned receipt identity"
      test -z "$pv_vg" || test "$pv_vg" = "$vg_name" || fail "${selected[$i]} belongs to foreign VG $pv_vg"
    else
      verify_or_blank_set
      test -z "$tag_owners" || fail "receipt PV ${selected[$i]} is missing from the tagged $vg_name VG"
      if vgs "$vg_name" >/dev/null 2>&1; then fail "receipt PV ${selected[$i]} is missing while $vg_name already exists"; fi
      pvcreate --yes --zero y --uuid "${planned_pv_uuid[$i]}" --norestorefile -- "${resolved[$i]}"
      pv=$(read_pv "${resolved[$i]}") || fail "pvcreate did not produce a readable PV for ${selected[$i]}"
      test "${pv%%|*}" = "${planned_pv_uuid[$i]}" && test -z "${pv#*|}" || fail "pvcreate identity mismatch for ${selected[$i]}"
    fi
    pv_done=$((i + 1))
    # Advance to pvs-created only while the VG is not yet receipt-bound. Once
    # the VG exists and its UUID is durable (vg-created/complete), a later
    # reapply sees every PV already present and must never regress the receipt:
    # a pvs-created stage with a non-empty vg_uuid is invalid and would brick
    # the next run. The pvs-created marker is only meaningful during the PV
    # creation transaction, before vgcreate (DES-HOR-545-03).
    if test -z "$receipt_vg_uuid"; then
      write_receipt pvs-created "$pv_done" 0; status=pvs-created; storage_stage_barrier "pv-$pv_done-created"
    fi
  done
fi

vg_exists=false
vg_record_count=$(vgs --noheadings --select "vg_name=$vg_name" -o vg_uuid 2>/dev/null | awk 'NF {n++} END {print n+0}')
test "$vg_record_count" -le 1 || fail "multiple VGs named $vg_name are ambiguous"
if test "$vg_record_count" = 1; then vg_exists=true; fi
if test "$vg_exists" = false; then
  test -z "$tag_owners" || fail "receipt ownership tag resolves to a missing or foreign VG"
  for ((i=0; i<count; i++)); do
    if pv=$(read_pv "${resolved[$i]}"); then
      test -z "${pv#*|}" || fail "receipt PV ${selected[$i]} names a VG that is not uniquely readable"
    fi
  done
  test "$mode" = inspect && { final_state="resumable-$status"; vg_size=0; vg_free=0; vg_uuid=; }
  if test "$mode" = reconcile; then
    test "$pv_done" = "$count" || fail "not every receipt PV is complete before vgcreate"
    verify_or_blank_set
    vgcreate --yes --addtag "$ownership_tag" "$vg_name" "${resolved[@]}"
    vg_exists=true
  fi
fi

if test "$vg_exists" = true; then
  vg_line=$(vgs --noheadings --separator '|' --units b --nosuffix -o vg_uuid,vg_tags,vg_size,vg_free,lv_count,pv_count "$vg_name") || fail "cannot inspect $vg_name"
  test "$(printf '%%s\n' "$vg_line" | awk 'NF {n++} END {print n+0}')" = 1 || fail "$vg_name identity is ambiguous"
  IFS='|' read -r vg_uuid vg_tags vg_size vg_free lv_count pv_count <<<"$vg_line"
  vg_uuid=$(printf '%%s' "$vg_uuid" | awk '{$1=$1;print}'); vg_tags=$(printf '%%s' "$vg_tags" | awk '{$1=$1;print}')
  vg_size=$(printf '%%s' "$vg_size" | awk '{$1=$1;printf "%%.0f",$1}'); vg_free=$(printf '%%s' "$vg_free" | awk '{$1=$1;printf "%%.0f",$1}')
  lv_count=$(printf '%%s' "$lv_count" | awk '{$1=$1;print}'); pv_count=$(printf '%%s' "$pv_count" | awk '{$1=$1;print}')
  valid_lvm_uuid "$vg_uuid" || fail "$vg_name reported an invalid VG UUID"
  test "$vg_tags" = "$ownership_tag" || fail "$vg_name ownership tag differs from the receipt"
  tag_owners=$(ownership_tag_owners)
  test "$tag_owners" = "$vg_name|$vg_uuid" || fail "$vg_name ownership tag is not globally unique"
  unexpected_segments=$(lvs --noheadings --select "vg_name=$vg_name" -o segtype | awk '{$1=$1; if(NF && $1 != "linear") print}')
  test -z "$unexpected_segments" || fail "$vg_name contains unsupported thin/snapshot/non-linear logical volumes: $unexpected_segments"
  test "$pv_count" = "$count" || fail "$vg_name PV membership count differs from the receipt"
  actual_members=$(pvs --noheadings --select "vg_name=$vg_name" -o pv_name | awk '{$1=$1; if(NF)print}' | sort)
  expected_members=$(printf '%%s\n' "${resolved[@]}" | sort)
  test "$actual_members" = "$expected_members" || fail "$vg_name PV membership differs from the selected set"
  for ((i=0; i<count; i++)); do
    pv=$(read_pv "${resolved[$i]}") || fail "receipt PV ${selected[$i]} disappeared"
    test "${pv%%|*}" = "${planned_pv_uuid[$i]}" && test "${pv#*|}" = "$vg_name" || fail "receipt PV ${selected[$i]} identity/membership drift"
  done
  if test -n "$receipt_vg_uuid"; then
    test "$vg_uuid" = "$receipt_vg_uuid" || fail "$vg_name UUID differs from the receipt"
  else
    test "$status" = pvs-created || fail "$vg_name exists before a valid receipt stage"
    if test "$mode" = reconcile; then
      receipt_vg_uuid=$vg_uuid; write_receipt vg-created "$count" 0; status=vg-created; storage_stage_barrier vg-created
    fi
  fi
  if test "$mode" = reconcile && test "$status" != complete; then
    write_receipt complete "$count" 0; status=complete; storage_stage_barrier complete
  fi
  if test "$status" = complete; then final_state=complete; else final_state="resumable-$status"; fi
fi

for ((i=0; i<count; i++)); do printf 'FORGE_DATA_STORAGE_DEVICE_RESULT\t%%s\t%%s\t%%s\t%%s\t%%s\t%%s\t%%s\t%%s\n' "${selected[$i]}" "${resolved[$i]}" "${model[$i]}" "${serial[$i]}" "${wwn[$i]}" "${planned_pv_uuid[$i]}" "${size[$i]}" "${transport[$i]}"; done
printf 'FORGE_DATA_STORAGE_RESULT\t%%s\t%%s\t%%s\t%%s\t%%s\t%%s\n' "$final_state" "$vg_name" "${vg_uuid:-}" "${vg_size:-0}" "${vg_free:-0}" "$count"
`, shellQuote(spec.InstallName), shellQuote(mode), shellQuote(dataStorageContractVersion), shellQuote(dataStorageReceiptPath), shellQuote(provisioner.DataVolumeGroupName), strings.Join(quoted, " "), dataStorageDeviceResolutionPrelude(), dataStorageReceiptWriterPrelude(), dataStorageStageBarrierPrelude(), dataStorageReceiptPrelude())
}

// WaitForLVMStorageReady validates the chart-owned substrate and exact VG
// discovery after the companion release is applied.
func (p *SSHProvisioner) WaitForLVMStorageReady(ctx context.Context, namespace string, host *provisioner.DataStorageState) (*provisioner.LVMStorageReadiness, error) {
	if host == nil || host.VGName != provisioner.DataVolumeGroupName || host.VGUUID == "" {
		return nil, fmt.Errorf("host data-storage identity is incomplete")
	}
	script := fmt.Sprintf(`
namespace=%s
expected_vg=%s
expected_uuid=%s
expected_size=%s
expected_free=%s
expected_pv_count=%s
fail() { printf 'lvm-storage readiness refusal: %%s\n' "$*" >&2; exit 42; }
csi_registered() {
  cluster_nodes=$(k3s kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null) || return 1
  test "$(printf '%%s\n' "$cluster_nodes" | awk 'NF {n++} END {print n+0}')" = 1 || return 1
  cluster_node=$(printf '%%s\n' "$cluster_nodes" | awk 'NF {print; exit}')
  registration=$(k3s kubectl get csinode "$cluster_node" -o go-template='{{range .spec.drivers}}{{if eq .name "local.csi.openebs.io"}}{{.name}}{{"\t"}}{{.nodeID}}{{"\n"}}{{range .topologyKeys}}{{.}}{{"\n"}}{{end}}{{end}}{{end}}' 2>/dev/null) || return 1
  csi_line=$(printf '%%s\n' "$registration" | awk 'NR == 1')
  IFS=$'\t' read -r csi_name csi_node_id <<<"$csi_line"
  test "$csi_name" = local.csi.openebs.io && test "$csi_node_id" = "$cluster_node" || return 1
  csi_keys=$(printf '%%s\n' "$registration" | awk 'NR > 1 && NF' | sort)
  test "$csi_keys" = "$(printf 'kubernetes.io/hostname\nopenebs.io/nodename')"
}
observed_node=; observed_size=; observed_free=; observed_lv_count=; observed_pv_count=; last_vg=; last_missing=; last_thin=
# OpenEBS LVMNode reports VG capacity in IEC units with a suffix (e.g. 25596Mi).
# Convert to bytes so capacity can be compared against the Forge receipt, which
# is expressed in bytes. Pure shell arithmetic avoids fmt percent handling.
to_bytes() {
  local v="$1" n
  n=$(echo "$v" | tr -cd '0-9')
  case "$v" in
    *K*) echo $((n * 1024)) ;;
    *M*) echo $((n * 1048576)) ;;
    *G*) echo $((n * 1073741824)) ;;
    *T*) echo $((n * 1099511627776)) ;;
    *P*) echo $((n * 1125899906842624)) ;;
    *) echo "$n" ;;
  esac
}
# OpenEBS LVMNode reports VG capacity that may differ from the Forge receipt by
# LVM metadata reserve / rounding, but must never contradict it. cap_ok allows a
# small absolute+relative tolerance so gross or unit-scale contradiction is still
# rejected while agent rounding is tolerated.
cap_ok() {
  awk -v e="$1" -v o="$2" -v abs=67108864 -v rel=0.02 'BEGIN{d=o-e; if(d<0)d=-d; if(e<=0){print (o>0)?1:0; exit} limit=(e*rel>abs?e*rel:abs); print (d<=limit)?1:0}'
}
auth_exact() {
  local identity="$1" verb="$2" expected="$3" output rc
  if output=$(k3s kubectl auth can-i "$verb" lvmsnapshots.local.openebs.io --all-namespaces --as="$identity" 2>/dev/null); then rc=0; else rc=$?; fi
  test "$output" = "$expected" || return 1
  if test "$expected" = yes; then test "$rc" = 0; else test "$rc" = 1; fi
}
snapshot_boundary_satisfies() {
  local observed crd resource policy binding identity verb instances controller_containers
  test ! -e /etc/modules-load.d/iterabase-data.conf || return 1
  ! grep -q '^dm_snapshot ' /proc/modules || return 1
  observed=$(k3s kubectl get crd lvmsnapshots.local.openebs.io -o name 2>/dev/null) || return 1
  test "$observed" = customresourcedefinition.apiextensions.k8s.io/lvmsnapshots.local.openebs.io || return 1
  test "$(k3s kubectl get crd lvmsnapshots.local.openebs.io -o jsonpath='{.status.conditions[?(@.type=="Established")].status}' 2>/dev/null)" = True || return 1
  instances=$(k3s kubectl get lvmsnapshots.local.openebs.io -A -o name 2>/dev/null) || return 1
  test -z "$instances" || return 1
  for crd in volumesnapshotclasses.snapshot.storage.k8s.io volumesnapshotcontents.snapshot.storage.k8s.io volumesnapshots.snapshot.storage.k8s.io; do
    observed=$(k3s kubectl get crd "$crd" --ignore-not-found=true -o name 2>/dev/null) || return 1
    test -z "$observed" || return 1
  done
  for resource in clusterrole/openebs-lvm-snapshotter-role clusterrolebinding/openebs-lvm-snapshotter-binding; do
    observed=$(k3s kubectl get "$resource" --ignore-not-found=true -o name 2>/dev/null) || return 1
    test -z "$observed" || return 1
  done
  policy=$(k3s kubectl get validatingadmissionpolicy iterabase-lvmsnapshot-create-deny -o jsonpath='{.spec.failurePolicy}|{.spec.matchConstraints.resourceRules[0].apiGroups[0]}|{.spec.matchConstraints.resourceRules[0].apiVersions[0]}|{.spec.matchConstraints.resourceRules[0].operations[0]}|{.spec.matchConstraints.resourceRules[0].resources[0]}|{.spec.validations[0].expression}' 2>/dev/null) || return 1
  test "$policy" = 'Fail|local.openebs.io|v1alpha1|CREATE|lvmsnapshots|false' || return 1
  binding=$(k3s kubectl get validatingadmissionpolicybinding iterabase-lvmsnapshot-create-deny -o jsonpath='{.spec.policyName}|{.spec.validationActions[0]}' 2>/dev/null) || return 1
  test "$binding" = 'iterabase-lvmsnapshot-create-deny|Deny' || return 1
  for identity in "system:serviceaccount:$namespace:openebs-lvm-controller-sa" "system:serviceaccount:$namespace:openebs-lvm-node-sa"; do
    for verb in list watch; do auth_exact "$identity" "$verb" yes || return 1; done
    for verb in get create update patch delete; do auth_exact "$identity" "$verb" no || return 1; done
  done
  controller_containers=$(k3s kubectl get deployment -n "$namespace" -l app=openebs-lvm-controller -o jsonpath='{range .items[*].spec.template.spec.containers[*]}{.name}{" "}{.image}{"\n"}{end}' 2>/dev/null) || return 1
  ! printf '%%s\n' "$controller_containers" | grep -qi snapshot
}
lvmnode_satisfies() {
  node_count=$(k3s kubectl get lvmnodes.local.openebs.io -n "$namespace" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | awk '{print NF}')
  test "$node_count" = 1 || return 1
  observed_node=$(k3s kubectl get lvmnodes.local.openebs.io -n "$namespace" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
  # Core fields match the base field set that validates on the real fixture;
  # optional probe fields are read separately so a null/missing probe can never
  # blank the authoritative VG line.
  vg=$(k3s kubectl get lvmnodes.local.openebs.io -n "$namespace" -o jsonpath='{range .items[0].volumeGroups[?(@.name=="iterabase-data")]}{.name}|{.uuid}|{.size}|{.free}|{.lvCount}|{.pvCount}{"\n"}{end}' 2>/dev/null)
  test -n "$vg" || return 1
  last_vg=$vg
  IFS='|' read -r vg_name vg_uuid vg_size vg_free lv_count pv_count <<<"$vg"
  test "$vg_name" = "$expected_vg" && test "$vg_uuid" = "$expected_uuid" || return 1
  test -n "$vg_size" && test -n "$vg_free" || return 1
  vg_size=$(to_bytes "$vg_size"); vg_free=$(to_bytes "$vg_free")
  for number in "$lv_count" "$pv_count" "$vg_size" "$vg_free"; do case "$number" in ''|*[!0-9]*) return 1 ;; esac; done
  # Capacity must be present, self-consistent (free<=size), and within tolerance
  # of the Forge receipt; UUID and membership are always exact.
  test "$(cap_ok "$expected_size" "$vg_size")" = 1 || return 1
  test "$(cap_ok "$expected_free" "$vg_free")" = 1 || return 1
  test "$vg_size" -ge "$vg_free" || return 1
  test "$pv_count" = "$expected_pv_count" || return 1
  # Probe fields are read alone; an absent field reads empty and is treated as
  # the healthy empty/zero state rather than blanking the whole VG read above.
  missing_pv=$(k3s kubectl get lvmnodes.local.openebs.io -n "$namespace" -o jsonpath='{range .items[0].volumeGroups[?(@.name=="iterabase-data")]}{.missingPvCount}{"\n"}{end}' 2>/dev/null | tr -d '[:space:]')
  last_missing=$missing_pv
  test "$missing_pv" = 0 || test -z "$missing_pv" || return 1
  thin_count=$(k3s kubectl get lvmnodes.local.openebs.io -n "$namespace" -o jsonpath='{range .items[0].volumeGroups[?(@.name=="iterabase-data")]}{len .thinPools}{"\n"}{end}' 2>/dev/null | tr -d '[:space:]')
  last_thin=$thin_count
  test "$thin_count" = 0 || test -z "$thin_count" || return 1
  observed_size=$vg_size; observed_free=$vg_free; observed_lv_count=$lv_count; observed_pv_count=$pv_count
  return 0
}
for attempt in $(seq 1 150); do
  if k3s kubectl get crd lvmnodes.local.openebs.io lvmvolumes.local.openebs.io >/dev/null 2>&1 &&
     k3s kubectl wait --for=condition=Established crd/lvmnodes.local.openebs.io crd/lvmvolumes.local.openebs.io --timeout=10s >/dev/null 2>&1 &&
     k3s kubectl wait -n "$namespace" --for=condition=Available deployment -l app=openebs-lvm-controller --timeout=10s >/dev/null 2>&1 &&
     k3s kubectl rollout status -n "$namespace" daemonset -l app=openebs-lvm-node --timeout=10s >/dev/null 2>&1 &&
     k3s kubectl get csidriver local.csi.openebs.io >/dev/null 2>&1 && csi_registered && snapshot_boundary_satisfies && lvmnode_satisfies; then
    break
  fi
  test "$attempt" -lt 150 || fail "OpenEBS LVM LocalPV volume CRDs/controller/node/CSI/classes, inert LVMSnapshot deny boundary, CSI/user snapshot absence, or receipt-matching iterabase-data VG discovery did not become Ready (last vg='$last_vg' missing='$last_missing' thin='$last_thin' obs_node='$observed_node')"
  sleep 2
done
classes=$(k3s kubectl get storageclass -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort)
test "$classes" = %s || fail "managed StorageClass set is not exactly the two approved classes: $classes"
test -z "$(k3s kubectl get storageclass -o jsonpath='{range .items[?(@.metadata.annotations.storageclass\\.kubernetes\\.io/is-default-class=="true")]}{.metadata.name}{"\n"}{end}')" || fail "a default StorageClass exists"
if k3s kubectl get deployment local-path-provisioner -n kube-system >/dev/null 2>&1 || k3s kubectl get configmap local-path-config -n kube-system >/dev/null 2>&1; then fail "K3s local-path provisioner still exists"; fi
verify_class() {
  name=$1; shared=$2
  observed=$(k3s kubectl get storageclass "$name" -o jsonpath='{.provisioner}|{.reclaimPolicy}|{.volumeBindingMode}|{.allowVolumeExpansion}|{.parameters.storage}|{.parameters.vgpattern}|{.parameters.fsType}|{.parameters.thinProvision}|{.parameters.shared}')
  test "$observed" = "local.csi.openebs.io|Delete|WaitForFirstConsumer|true|lvm|^iterabase-data$|xfs|no|$shared" || fail "StorageClass $name contract drift: $observed"
}
verify_class %s no
verify_class %s yes
test -n "$observed_node" || fail "LVMNode identity is missing"
printf 'FORGE_LVM_STORAGE_READY\t%%s\t%%s\t%%s\t%%s\t%%s\t%%s\t%%s\n' "$observed_node" "$expected_vg" "$expected_uuid" "$observed_size" "$observed_free" "$observed_lv_count" "$observed_pv_count"
`, shellQuote(namespace), shellQuote(host.VGName), shellQuote(host.VGUUID), shellQuote(strconv.FormatUint(host.SizeBytes, 10)), shellQuote(strconv.FormatUint(host.FreeBytes, 10)), shellQuote(strconv.Itoa(len(host.Devices))), shellQuote(provisioner.AgentPoolStorageClass+"\n"+provisioner.PlatformStorageClass), shellQuote(provisioner.PlatformStorageClass), shellQuote(provisioner.AgentPoolStorageClass))
	out, err := p.run(ctx, "sudo bash -ceu "+shellQuote(script))
	if err != nil {
		return nil, fmt.Errorf("wait for LVM storage substrate: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 8 || parts[0] != "FORGE_LVM_STORAGE_READY" {
			continue
		}
		size, parseErr := strconv.ParseUint(parts[4], 10, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("parse LVMNode size: %w", parseErr)
		}
		free, parseErr := strconv.ParseUint(parts[5], 10, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("parse LVMNode free size: %w", parseErr)
		}
		lvCount, parseErr := strconv.Atoi(parts[6])
		if parseErr != nil {
			return nil, fmt.Errorf("parse LVMNode LV count: %w", parseErr)
		}
		pvCount, parseErr := strconv.Atoi(parts[7])
		if parseErr != nil {
			return nil, fmt.Errorf("parse LVMNode PV count: %w", parseErr)
		}
		return &provisioner.LVMStorageReadiness{Ready: true, NodeName: parts[1], VGName: parts[2], VGUUID: parts[3], SizeBytes: size, FreeBytes: free, LVCount: lvCount, PVCount: pvCount}, nil
	}
	return nil, fmt.Errorf("LVM storage readiness returned no bounded result")
}

// dataStorageReceiptWriterPrelude emits the one atomic+fsynced receipt writer
// shared by reconcile and purge. purge_done is an explicit monotonic count used
// only by the purge transaction; ordinary reconcile always writes zero.
func dataStorageReceiptWriterPrelude() string {
	return `
write_receipt() {
  local status_value=$1 pv_done_value=$2 purge_done_value=$3 receipt_dir tmp r
  receipt_dir=$(dirname "$receipt")
  install -d -o root -g root -m 0700 "$receipt_dir"
  umask 077; tmp=$(mktemp "$receipt_dir/.data-storage.receipt.XXXXXX")
  {
    printf 'contract=%s\nstatus=%s\npv_done=%s\npurge_done=%s\n' "$contract" "$status_value" "$pv_done_value" "$purge_done_value"
    printf 'install_b64=%s\n' "$(printf '%s' "$install_name" | base64 -w0)"
    printf 'device_count=%s\nvg_name=%s\nownership_tag=%s\nvg_uuid=%s\n' "$count" "$vg_name" "$ownership_tag" "$receipt_vg_uuid"
    for ((r=0; r<count; r++)); do
      printf 'device_%s_b64=%s\n' "$r" "$(printf '%s' "${selected[$r]}" | base64 -w0)"
      printf 'resolved_%s_b64=%s\n' "$r" "$(printf '%s' "${resolved[$r]}" | base64 -w0)"
      printf 'model_%s_b64=%s\n' "$r" "$(printf '%s' "${model[$r]}" | base64 -w0)"
      printf 'serial_%s_b64=%s\n' "$r" "$(printf '%s' "${serial[$r]}" | base64 -w0)"
      printf 'wwn_%s_b64=%s\n' "$r" "$(printf '%s' "${wwn[$r]}" | base64 -w0)"
      printf 'transport_%s_b64=%s\n' "$r" "$(printf '%s' "${transport[$r]}" | base64 -w0)"
      printf 'size_%s=%s\npv_uuid_%s=%s\n' "$r" "${size[$r]}" "$r" "${planned_pv_uuid[$r]}"
    done
  } > "$tmp"
  chown root:root "$tmp"; chmod 0600 "$tmp"; sync -f "$tmp"; mv -f "$tmp" "$receipt"; sync -f "$receipt_dir"
}
`
}

// dataStorageStageBarrierPrelude is inert in every production invocation. The
// privileged executable fault harness supplies both internal fixture variables
// to stop synchronously after an exact durable stage, inspect receipt/live LVM
// state, and kill the whole process group without polling transient strings.
func dataStorageStageBarrierPrelude() string {
	return `
storage_stage_barrier() {
  local stage=$1 reached release attempt
  test "${FORGE_DATA_STORAGE_FIXTURE_LOOP:-}" = 1 || return 0
  test -n "${FORGE_DATA_STORAGE_STAGE_BARRIER_DIR:-}" || return 0
  test "$stage" = "${FORGE_DATA_STORAGE_STAGE_BARRIER:-}" || return 0
  printf '%s' "$stage" | grep -Eq '^[a-z0-9-]+$' || fail "invalid internal storage stage barrier"
  test -d "$FORGE_DATA_STORAGE_STAGE_BARRIER_DIR" || fail "internal storage stage barrier directory is unavailable"
  reached="$FORGE_DATA_STORAGE_STAGE_BARRIER_DIR/$stage.reached"
  release="$FORGE_DATA_STORAGE_STAGE_BARRIER_DIR/$stage.release"
  umask 077; printf '%s\n' "$stage" > "$reached"; sync -f "$reached"; sync -f "$FORGE_DATA_STORAGE_STAGE_BARRIER_DIR"
  for attempt in $(seq 1 9000); do
    test ! -e "$release" || return 0
    sleep 0.01
  done
  fail "internal storage stage barrier $stage timed out"
}
`
}

// dataStorageReceiptPrelude emits the shared read-only receipt-parsing and
// identity-validation prelude used by both exact reconcile and explicit purge
// (DES-HOR-545-03). It defines the receipt accessors, loads the durable receipt
// (when present) into status/pv_done/receipt_vg_uuid/ownership_tag/
// planned_pv_uuid, sets receipt_present, and fails closed on any identity,
// membership, hardware, or stage drift. It performs no mutation, so the two
// lifecycle paths can never diverge on what state they accept. Callers layer
// their reconcile/purge mutations on top of this read-only prelude.
func dataStorageReceiptPrelude() string {
	return `
receipt_value() { awk -F= -v wanted="$1" '$1 == wanted {sub(/^[^=]*=/, ""); print; found=1} END {if (!found) exit 3}' "$receipt"; }
decode_receipt() { receipt_value "$1" | base64 -d; }
valid_lvm_uuid() { case "$1" in ??????-????-????-????-????-????-??????) return 0 ;; *) return 1 ;; esac; }
valid_ownership_tag() { printf '%s' "$1" | grep -Eq '^iterabase[.]hor545[.][0-9a-f]{32}$'; }
status=; pv_done=0; purge_done=0; receipt_vg_uuid=; ownership_tag=; planned_pv_uuid=(); receipt_present=0
if test -e "$receipt"; then
  receipt_present=1
  test -f "$receipt" && test ! -L "$receipt" || fail "data-storage receipt is not a regular file"
  test "$(stat -c '%u:%g:%a' "$receipt")" = 0:0:600 || fail "data-storage receipt ownership/mode drift"
  test "$(receipt_value contract)" = "$contract" || fail "data-storage receipt contract mismatch"
  status=$(receipt_value status); case "$status" in planned|pvs-created|vg-created|complete|purge-vg-pending|purge-vg-removed|purge-pv-pending|purge-pvs-removed|purge-receipt-pending) ;; *) fail "data-storage receipt status is invalid" ;; esac
  pv_done=$(receipt_value pv_done); case "$pv_done" in ''|*[!0-9]*) fail "data-storage receipt pv_done is invalid" ;; esac
  purge_done=$(receipt_value purge_done 2>/dev/null || printf 0); case "$purge_done" in ''|*[!0-9]*) fail "data-storage receipt purge_done is invalid" ;; esac
  test "$pv_done" -le "$count" || fail "data-storage receipt pv_done exceeds device count"
  test "$purge_done" -le "$count" || fail "data-storage receipt purge_done exceeds device count"
  test "$(decode_receipt install_b64)" = "$install_name" || fail "data-storage receipt install mismatch"
  test "$(receipt_value device_count)" = "$count" || fail "data-storage configured device-set size differs from the receipt"
  test "$(receipt_value vg_name)" = "$vg_name" || fail "data-storage VG name mismatch"
  ownership_tag=$(receipt_value ownership_tag)
  valid_ownership_tag "$ownership_tag" || fail "data-storage receipt ownership tag is invalid"
  receipt_vg_uuid=$(receipt_value vg_uuid)
  case "$status" in
    planned)
      test "$pv_done" = 0 && test "$purge_done" = 0 || fail "data-storage planned receipt has completed transaction stages"
      test -z "$receipt_vg_uuid" || fail "data-storage receipt records a VG UUID before the VG-created stage"
      ;;
    pvs-created)
      test "$pv_done" -gt 0 && test "$purge_done" = 0 || fail "data-storage pvs-created receipt has invalid transaction progress"
      test -z "$receipt_vg_uuid" || fail "data-storage receipt records a VG UUID before the VG-created stage"
      ;;
    vg-created|complete)
      test "$pv_done" = "$count" && test "$purge_done" = 0 || fail "data-storage VG receipt does not bind the complete PV set"
      valid_lvm_uuid "$receipt_vg_uuid" || fail "data-storage receipt VG UUID is invalid"
      ;;
    purge-vg-pending|purge-vg-removed|purge-pv-pending|purge-pvs-removed|purge-receipt-pending)
      test -z "$receipt_vg_uuid" || valid_lvm_uuid "$receipt_vg_uuid" || fail "data-storage purge receipt VG UUID is invalid"
      test "$status" != purge-vg-pending && test "$status" != purge-vg-removed || test "$purge_done" = 0 || fail "data-storage VG purge stage has removed PV progress"
      test "$status" != purge-receipt-pending || test "$purge_done" = "$pv_done" || fail "data-storage receipt removal stage precedes PV removal"
      ;;
  esac
  for ((i=0; i<count; i++)); do
    test "$(decode_receipt device_${i}_b64)" = "${selected[$i]}" || fail "data-storage device order/set differs from the receipt"
    # resolved_<i> is recorded, not bound: kernel names (nvme0n1, sdb) are not
    # stable across boots, so identity rests on the by-id link, hardware, size,
    # and the receipt PV/VG UUIDs (DES-HOR-616-01).
    test "$(decode_receipt model_${i}_b64)" = "${model[$i]}" || fail "data-storage model identity drift"
    test "$(decode_receipt serial_${i}_b64)" = "${serial[$i]}" || fail "data-storage serial identity drift"
    test "$(decode_receipt wwn_${i}_b64)" = "${wwn[$i]}" || fail "data-storage WWN identity drift"
    test "$(decode_receipt transport_${i}_b64)" = "${transport[$i]}" || fail "data-storage transport identity drift"
    test "$(receipt_value size_$i)" = "${size[$i]}" || fail "data-storage size identity drift"
    planned_pv_uuid[$i]=$(receipt_value pv_uuid_$i)
    valid_lvm_uuid "${planned_pv_uuid[$i]}" || fail "data-storage receipt PV UUID is invalid"
  done
fi
`
}
