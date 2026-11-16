-- Raspberry Pi boot configuration only; the player never edits device trees.
local fs = require 'nixio.fs'
local nixio = require 'nixio'
local json = require 'luci.jsonc'
local M = {}
local first, last = '# BEGIN Slopify HAT', '# END Slopify HAT'

local function read(path, limit)
	local f = io.open(path, 'rb')
	if not f then return nil end
	local data = f:read((limit or 65536) + 1) or ''
	f:close()
	assert(#data <= (limit or 65536), 'File too large: ' .. path)
	return data
end

local function text(path)
	return (read(path, 1024) or ''):match('^[^%z]*'):match('^%s*(.-)%s*$')
end

local function digest(data)
	-- Change token, not authentication. The rpcd ACL authorizes writes.
	local a, b = 1, 0
	for i = 1, #data do
		a = (a + data:byte(i)) % 65521
		b = (b + a) % 65521
	end
	return string.format('%04x%04x:%d', b, a, #data)
end

function M.overlays(dir)
	local names = {}
	for file in fs.dir(dir) do
		local name = file:match('^([%w_-]+)%.dtbo$')
		if name then
			local data = read(dir .. '/' .. file, 1048576)
			-- Same audio-property filter used by moOde; never list platform DTBs.
			if data and data:sub(1, 4) == '\208\13\254\237' and
				(data:find('#sound-dai-cells\0', 1, true) or name == 'cirrus-wm5102') then
				names[#names + 1] = name
			end
		end
	end
	table.sort(names)
	return names
end

local function contains(names, name)
	for _, value in ipairs(names) do if value == name then return true end end
	return false
end

-- Only edit simple audio declarations in config.txt. Leave unrelated settings
-- alone and refuse parameterised/conditional configurations instead of guessing.
function M.clean(data, names)
	local kept, selected, section, block, seen = {}, 'auto', 'all', false, false
	for line in (data:gsub('\n$', '') .. '\n'):gmatch('(.-)\n') do
		local s = line:match('^%s*(.-)%s*$')
		if s == first then
			assert(not block and not seen, 'Duplicate HAT block')
			block, seen = true, true
		elseif s == last then
			assert(block, 'Unmatched HAT block end')
			block = false
		else
			section = s:match('^%[(.-)%]$') or section
			assert(not s:match('^force_eeprom_read='), 'Custom EEPROM setting: edit config.txt manually')
			local value = s:match('^dtoverlay=(.*)$')
			local name = value and value:match('^([^,]*)')
			if name and (name == '' or contains(names, name)) then
				assert(section == 'all' and value == name, 'Conditional/parameterised audio overlay: edit config.txt manually')
				assert(selected == 'auto' or selected == 'off', 'Multiple audio overlays: edit config.txt manually')
				selected = name == '' and 'off' or name
			else
				assert(not block, 'Unexpected content in HAT block')
				kept[#kept + 1] = line
			end
		end
	end
	assert(not block, 'Unterminated HAT block')
	return table.concat(kept, '\n') .. '\n', selected
end

local function includes(data, boot, names, depth)
	assert(depth < 8, 'Too many boot includes')
	for line in (data .. '\n'):gmatch('(.-)\n') do
		local name = line:match('^%s*include%s+(%S+)')
		if name then
			assert(name:match('^[%w_.-]+$') and name ~= '..' and name ~= '.', 'Unsupported boot include')
			local child = assert(read(boot .. '/' .. name), 'Cannot read boot include')
			local _, selected = M.clean(child, names)
			assert(selected == 'auto', 'Audio configured in included file: edit it manually')
			includes(child, boot, names, depth + 1)
		end
	end
end

function M.rewrite(data, names, choice)
	assert(choice == 'auto' or choice == 'off' or contains(names, choice), 'Choose an installed audio overlay')
	local base = M.clean(data, names)
	local block = first .. '\n'
	if choice ~= 'auto' then
		-- Must precede every dtoverlay/dtparam, including distroconfig.txt.
		-- Empty dtoverlay suppresses HAT overlay loading, not EEPROM identity.
		block = block .. 'dtoverlay=\n'
		if choice ~= 'off' then block = block .. 'dtoverlay=' .. choice .. '\n' end
	end
	return block .. last .. '\n' .. base
end

function M.info(boot, dt)
	local result = { available = false, overlays = {}, selected = 'auto' }
	if not text(dt .. '/model'):match('^Raspberry Pi') or not fs.access(boot .. '/config.txt') then return result end
	result.available = true
	result.product = text(dt .. '/hat/product')
	result.vendor = text(dt .. '/hat/vendor')
	result.eeprom = result.product ~= ''
	local ok, err = pcall(function()
		result.overlays = M.overlays(boot .. '/overlays')
		local data = assert(read(boot .. '/config.txt'), 'Cannot read config.txt')
		result.revision = digest(data)
		local _, selected = M.clean(data, result.overlays)
		result.selected = selected
		includes(data, boot, result.overlays, 0)
		-- Sound name is the currently loaded device, not a prediction for an
		-- EEPROM override while an explicit overlay is active.
		if selected == 'auto' and result.eeprom then
			result.active = text(dt .. '/sound/name')
			if result.active == '' then result.active = text(dt .. '/sound/compatible') end
		end
	end)
	if not ok then result.error = tostring(err) end
	return result
end

local function atomic(path, data)
	local tmp = path .. '.slopify-tmp'
	local f = assert(nixio.open(tmp, 'w', '600'), 'Cannot create boot temporary file')
	local ok, err = pcall(function()
		assert(f:writeall(data), 'Cannot write boot configuration')
		assert(f:sync(), 'Cannot sync boot configuration')
	end)
	f:close()
	if not ok then fs.unlink(tmp); error(err) end
	assert(fs.rename(tmp, path), 'Cannot replace boot configuration')
	local dir = assert(nixio.open(path:match('^(.*)/'), 'r'))
	local synced = dir:sync()
	dir:close()
	assert(synced, 'Cannot sync boot directory')
end

function M.set(boot, dt, choice, revision)
	local lock = assert(nixio.open('/var/lock/slopify-hat.lock', 'w', '600'))
	assert(lock:lock('tlock'), 'Another HAT update is in progress')
	local ok, err = pcall(function()
		local info = M.info(boot, dt)
		assert(info.available, 'Raspberry Pi boot configuration unavailable')
		assert(not info.error, info.error)
		assert(info.revision == revision, 'Boot configuration changed; reload the page')
		local data = assert(read(boot .. '/config.txt'))
		local next = M.rewrite(data, info.overlays, choice)
		if data ~= next then
			atomic(boot .. '/config.txt.slopify-backup', data)
			atomic(boot .. '/config.txt', next)
		end
	end)
	lock:close()
	assert(ok, err)
	return M.info(boot, dt)
end

function M.run(action, choice, revision)
	local ok, value = pcall(function()
		if action == 'info' then return M.info('/boot', '/sys/firmware/devicetree/base') end
		assert(action == 'set' and choice and revision, 'Expected overlay and revision')
		return M.set('/boot', '/sys/firmware/devicetree/base', choice, revision)
	end)
	if not ok then io.stderr:write(tostring(value), '\n'); os.exit(1) end
	print(json.stringify(value))
end

return M
