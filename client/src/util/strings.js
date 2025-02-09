// SPDX-FileCopyrightText: 2021 Softbear, Inc.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Pluralizes a word based on whether count is 1 or something else.
// Not guaranteed to work for cases not used by existing callers.
export function plural(str, count) {
	if (count == 1) {
		return str;
	}
	if (str.endsWith('ss')) {
		return str + 'es';
	}
	return str + 's';
}
