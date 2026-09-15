# syntax=docker/dockerfile:1

# ArduPilot SITL for KEEL's MAVLink adapter (spec.md section 7.4).
#
# The official static x86_64 SITL binaries ArduPilot publishes for each
# release, and the default parameters sim_vehicle.py loads with them, taken at
# the release's commit. Nothing is compiled here: the binaries are statically
# linked and run on a distroless base with no shell and no package manager.
#
# Release 4.7.1: commit dbe792162d06cab66c3475fd5556bf7a120f119e, tags
# Copter-4.7.1 and Rover-4.7.1. Every download is checked by sha256, so a
# release bump changes the URLs and the checksums together.

FROM gcr.io/distroless/static-debian13:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7

ADD --chmod=0755 --checksum=sha256:011627d41dd95640c3aca02d45283474be48721c8b1bb7a4b234d38b69063482 \
    https://firmware.ardupilot.org/Copter/stable-4.7.1/SITL_x86_64_linux_gnu/arducopter /opt/ardupilot/arducopter
ADD --chmod=0755 --checksum=sha256:ef27042b95711fb1f1a24c0c64da9934a2eb56ba81398cfd66d33f185177fb5a \
    https://firmware.ardupilot.org/Rover/stable-4.7.1/SITL_x86_64_linux_gnu/ardurover /opt/ardupilot/ardurover
ADD --chmod=0644 --checksum=sha256:5e01345b45d1c6190b28bece5638bbdd4cf1cce35e05bbbf480ab24d2b51aa0e \
    https://raw.githubusercontent.com/ArduPilot/ardupilot/dbe792162d06cab66c3475fd5556bf7a120f119e/Tools/autotest/default_params/copter.parm /opt/ardupilot/copter.parm
ADD --chmod=0644 --checksum=sha256:6479879d3e625854a6c32cb4d3fe231bab81d935e07904eac8a411fd6f971bc4 \
    https://raw.githubusercontent.com/ArduPilot/ardupilot/dbe792162d06cab66c3475fd5556bf7a120f119e/Tools/autotest/default_params/rover.parm /opt/ardupilot/rover.parm

# SITL writes its eeprom.bin and its logs to the working directory, which the
# nonroot user owns.
WORKDIR /home/nonroot
