package hpack

import "io"

// / constant u32 array
// / from https://github.com/on-keyday/utils/blob/main/src/include/fnet/util/hpack/hpack_huffman_table.h
var raw_table = [(257 << 1) - 1]HuffmanTree{
	229377, 65584, 1441795, 131250, 1343493, 884742, 2147483680, 262210, 295003, 327802, 360631, 393244, 426113, 589838, 1114127, 557072, 2147483681, 2147483682, 622623, 753684, 688319, 2686998, 2147483683, 2392088, 2752537, 852162, 2147483684, 2147483685, 1277981, 1212446, 2147483686, 1048659, 1245217, 2147483687, 1179683, 2147483688, 2147483689, 2147483690, 2147483691, 2359336, 2147483692, 1409066, 2147483693, 2147483694, 1933357, 1835054, 1802287, 2147483695, 4882481, 1736754, 1703987, 2147483696, 2147483697, 4816950, 2147483698, 2147483699, 1900601, 2147483700, 2147483701, 2064444, 2031677, 2147483702, 2147483703, 2129984, 2147483704, 2147483705, 2195534, 2228390, 2261168, 2883654, 2850887, 2147483706, 2147483707, 2424972, 2457742, 4390988, 4784205, 2147483708, 4980815, 4718672, 2818129, 2147483709, 2147483710, 2147483711, 4358229, 2147483712, 2147483713, 2147483714, 2949209, 2147483715, 2147483716, 3506268, 3276893, 3178590, 3145823, 2147483717, 2147483718, 3244130, 2147483719, 2147483720, 3407973, 3375206, 2147483721, 2147483722, 3473513, 2147483723, 2147483724, 3768428, 3670125, 3637358, 2147483725, 2147483726, 3735665, 2147483727, 2147483728, 3899508, 3866741, 2147483729, 2147483730, 3965048, 2147483731, 2147483732, 5308539, 4161660, 4128893, 2147483733, 2147483734, 4292736, 2147483735, 4325506, 2147483736, 2147483737, 2147483738, 2147483739, 4423870, 6389896, 9371785, 9044106, 13435019, 2147483740, 6324365, 2147483741, 6291599, 2147483742, 4849809, 2147483743, 2147483744, 2147483745, 2147483746, 5243030, 5079191, 2147483747, 5144729, 5111962, 2147483748, 2147483749, 2147483750, 5210270, 2147483751, 2147483752, 5636257, 2147483753, 5701795, 5406884, 2147483754, 2147483755, 5570727, 5537960, 2147483756, 2147483757, 5669035, 2147483758, 2147483759, 2147483760, 5963951, 2147483761, 5931185, 2147483762, 5898419, 2147483763, 2147483764, 2147483765, 2147483766, 6127800, 6095033, 2147483767, 2147483768, 6193340, 2147483769, 2147483770, 2147483771, 2147483772, 2147483773, 2147483774, 2147483648, 6422807, 6652101, 6488441, 6521246, 9863368, 9765065, 9666762, 2147483649, 6684986, 6717666, 6750570, 6783379, 6816184, 7569617, 7274706, 7045331, 6947326, 7012565, 2147483650, 2147483651, 7176408, 7143641, 2147483652, 2147483653, 7241948, 2147483654, 2147483655, 7962847, 7831776, 7799009, 2147483656, 7438732, 10125540, 7504358, 10060006, 2147483657, 7602426, 7635211, 7667986, 7700982, 8421612, 7897325, 2147483658, 2147483659, 7930096, 2147483660, 2147483661, 2147483662, 8093940, 8061173, 2147483663, 2147483664, 8159480, 2147483665, 2147483666, 8519931, 8356092, 8323325, 2147483667, 2147483668, 8487168, 2147483669, 16777474, 2147483670, 2147483671, 8651013, 8618246, 2147483672, 2147483673, 8716553, 2147483674, 2147483675, 8880396, 8847629, 2147483676, 2147483677, 8945936, 2147483678, 2147483679, 15040787, 2147483775, 9077172, 9339158, 2147483776, 9175412, 10879257, 9535770, 9273817, 9503004, 2147483777, 2147483778, 10748191, 12714272, 11305249, 2147483779, 2147483780, 9699620, 9634085, 2147483781, 2147483782, 2147483783, 10256681, 2147483784, 9830699, 2147483785, 2147483786, 9994542, 9961775, 2147483787, 2147483788, 10092850, 2147483789, 2147483790, 2147483791, 10453302, 10223927, 2147483792, 2147483793, 2147483794, 11469115, 10649916, 10551613, 10518846, 2147483795, 11174208, 2147483796, 2147483797, 10617155, 2147483798, 2147483799, 11075910, 11010375, 2147483800, 10781137, 11632970, 11272523, 2147483801, 11370829, 11206990, 11043151, 2147483802, 2147483803, 2147483804, 11141459, 2147483805, 2147483806, 2147483807, 11338071, 2147483808, 2147483809, 2147483810, 2147483811, 11796828, 11764061, 2147483812, 12091743, 11698528, 11600225, 2147483813, 2147483814, 11993444, 2147483815, 12058982, 2147483816, 2147483817, 12026217, 2147483818, 13664619, 14713196, 14221677, 2147483819, 2147483820, 2147483821, 2147483822, 12616050, 12550515, 2147483823, 14778741, 12484982, 12321143, 2147483824, 2147483825, 12878202, 12779899, 12583292, 2147483826, 14319998, 2147483827, 2147483828, 2147483829, 12681602, 2147483830, 2147483831, 13402501, 2147483832, 12845447, 2147483833, 2147483834, 13107594, 13074827, 2147483835, 13500813, 13173134, 2147483836, 2147483837, 13468049, 2147483838, 2147483839, 13992340, 13894037, 13795734, 13369751, 2147483840, 2147483841, 2147483842, 2147483843, 2147483844, 15532445, 2147483845, 15565215, 15434144, 2147483846, 13697505, 15663523, 14254500, 2147483847, 13861286, 2147483848, 2147483849, 14352809, 14188970, 2147483850, 14025159, 14057962, 14090751, 14156207, 2147483851, 2147483852, 2147483853, 2147483854, 2147483855, 2147483856, 2147483857, 14614967, 2147483858, 16187833, 15106490, 14647739, 14582204, 2147483859, 2147483860, 2147483861, 15073728, 2147483862, 15303106, 2147483863, 15368644, 14877125, 2147483864, 2147483865, 15860168, 15008201, 2147483866, 2147483867, 2147483868, 2147483869, 15991246, 15204815, 2147483870, 2147483871, 15335890, 2147483872, 2147483873, 2147483874, 15466966, 2147483875, 2147483876, 2147483877, 2147483878, 2147483879, 15630812, 2147483880, 2147483881, 15729119, 2147483882, 2147483883, 15827426, 2147483884, 2147483885, 15958501, 2147483886, 2147483887, 2147483888, 16155113, 2147483889, 16122347, 2147483890, 2147483891, 2147483892, 16482799, 16351728, 16318961, 2147483893, 2147483894, 16417268, 2147483895, 2147483896, 2147483897, 16613880, 16581113, 2147483898, 2147483899, 16679420, 2147483900, 2147483901, 2147483902, 2147483903, 2147483904,
}

// static constexpr std::uint32_t mask_zero = 0x00007fff;
const mask_zero = 0x00007fff

// static constexpr std::uint32_t mask_one = 0x3fff8000;
const mask_one = 0x3fff8000

// static constexpr std::uint32_t flag_value = 0x80000000;
const flag_value = 0x80000000

// static constexpr std::uint32_t mask_shift = 15;
const mask_shift = 15

// / from https://github.com/on-keyday/utils/blob/main/src/include/file/gzip/huffman.h#L122 DecodeTree
type HuffmanTree uint32

func (h HuffmanTree) OneIndex() uint16 {
	return uint16((uint32(h) & mask_one) >> mask_shift)
}
func (h HuffmanTree) ZeroIndex() uint16 {
	return uint16(h & mask_zero)
}
func (h HuffmanTree) HasValue() bool {
	return h&flag_value != 0
}
func (h HuffmanTree) GetValue() uint32 {
	return uint32(h & 0x7fffffff)
}

func GetRoot() HuffmanTree {
	return raw_table[0]
}

func (h HuffmanTree) Next(bit bool) (HuffmanTree, bool) {
	if h.HasValue() {
		return HuffmanTree(0), false
	}
	var nextIndex uint16
	if bit {
		nextIndex = h.OneIndex()
	} else {
		nextIndex = h.ZeroIndex()
	}
	if nextIndex == 0 || nextIndex >= uint16(len(raw_table)) {
		return HuffmanTree(0), false
	}
	return raw_table[nextIndex], true
}

type Code struct {
	Code    uint32
	Bits    uint8
	Literal uint16
}

type BitWriter struct {
	bits uint8
	buf  []byte
}

func (c *BitWriter) WriteBit(bit bool) {
	bytes := uint8(0)
	if bit {
		bytes = 1
	}
	if c.bits == 0 {
		c.buf = append(c.buf, 0)
		c.bits = 0
	}
	c.buf[len(c.buf)-1] |= bytes << (7 - c.bits)
	c.bits++
	c.bits &= 7
}

func (c *BitWriter) Fill() {
	for c.bits != 0 {
		c.WriteBit(true)
	}
}

type BitReader struct {
	buf  []byte
	pos  int
	bits uint8
}

func (r *BitReader) ReadBit() (bool, error) {
	if r.pos >= len(r.buf) {
		return false, io.EOF
	}
	bit := (r.buf[r.pos] & (1 << (7 - r.bits))) != 0
	r.bits++
	if r.bits == 8 {
		r.bits = 0
		r.pos++
	}
	return bit, nil
}

func (c *Code) Write(w *BitWriter) error {
	//auto b = std::uint64_t(1) << (bits - 1);
	//for (auto i = 0; i < bits; i++) {
	//    out.push_back(code & b ? t : f);
	//    if (b == 1) {
	//        break;
	//    }
	//    b >>= 1;
	//}
	//return true;
	var b uint32 = uint32(1) << (c.Bits - 1)
	for i := 0; i < int(c.Bits); i++ {
		w.WriteBit((c.Code & b) != 0)
		if b == 1 {
			break
		}
		b >>= 1
	}
	return nil
}

var Codes = [257]Code{
	{Code: 8184, Bits: 13, Literal: 0},
	{Code: 8388568, Bits: 23, Literal: 1},
	{Code: 268435426, Bits: 28, Literal: 2},
	{Code: 268435427, Bits: 28, Literal: 3},
	{Code: 268435428, Bits: 28, Literal: 4},
	{Code: 268435429, Bits: 28, Literal: 5},
	{Code: 268435430, Bits: 28, Literal: 6},
	{Code: 268435431, Bits: 28, Literal: 7},
	{Code: 268435432, Bits: 28, Literal: 8},
	{Code: 16777194, Bits: 24, Literal: 9},
	{Code: 1073741820, Bits: 30, Literal: 10},
	{Code: 268435433, Bits: 28, Literal: 11},
	{Code: 268435434, Bits: 28, Literal: 12},
	{Code: 1073741821, Bits: 30, Literal: 13},
	{Code: 268435435, Bits: 28, Literal: 14},
	{Code: 268435436, Bits: 28, Literal: 15},
	{Code: 268435437, Bits: 28, Literal: 16},
	{Code: 268435438, Bits: 28, Literal: 17},
	{Code: 268435439, Bits: 28, Literal: 18},
	{Code: 268435440, Bits: 28, Literal: 19},
	{Code: 268435441, Bits: 28, Literal: 20},
	{Code: 268435442, Bits: 28, Literal: 21},
	{Code: 1073741822, Bits: 30, Literal: 22},
	{Code: 268435443, Bits: 28, Literal: 23},
	{Code: 268435444, Bits: 28, Literal: 24},
	{Code: 268435445, Bits: 28, Literal: 25},
	{Code: 268435446, Bits: 28, Literal: 26},
	{Code: 268435447, Bits: 28, Literal: 27},
	{Code: 268435448, Bits: 28, Literal: 28},
	{Code: 268435449, Bits: 28, Literal: 29},
	{Code: 268435450, Bits: 28, Literal: 30},
	{Code: 268435451, Bits: 28, Literal: 31},
	{Code: 20, Bits: 6, Literal: 32},
	{Code: 1016, Bits: 10, Literal: 33},
	{Code: 1017, Bits: 10, Literal: 34},
	{Code: 4090, Bits: 12, Literal: 35},
	{Code: 8185, Bits: 13, Literal: 36},
	{Code: 21, Bits: 6, Literal: 37},
	{Code: 248, Bits: 8, Literal: 38},
	{Code: 2042, Bits: 11, Literal: 39},
	{Code: 1018, Bits: 10, Literal: 40},
	{Code: 1019, Bits: 10, Literal: 41},
	{Code: 249, Bits: 8, Literal: 42},
	{Code: 2043, Bits: 11, Literal: 43},
	{Code: 250, Bits: 8, Literal: 44},
	{Code: 22, Bits: 6, Literal: 45},
	{Code: 23, Bits: 6, Literal: 46},
	{Code: 24, Bits: 6, Literal: 47},
	{Code: 0, Bits: 5, Literal: 48},
	{Code: 1, Bits: 5, Literal: 49},
	{Code: 2, Bits: 5, Literal: 50},
	{Code: 25, Bits: 6, Literal: 51},
	{Code: 26, Bits: 6, Literal: 52},
	{Code: 27, Bits: 6, Literal: 53},
	{Code: 28, Bits: 6, Literal: 54},
	{Code: 29, Bits: 6, Literal: 55},
	{Code: 30, Bits: 6, Literal: 56},
	{Code: 31, Bits: 6, Literal: 57},
	{Code: 92, Bits: 7, Literal: 58},
	{Code: 251, Bits: 8, Literal: 59},
	{Code: 32764, Bits: 15, Literal: 60},
	{Code: 32, Bits: 6, Literal: 61},
	{Code: 4091, Bits: 12, Literal: 62},
	{Code: 1020, Bits: 10, Literal: 63},
	{Code: 8186, Bits: 13, Literal: 64},
	{Code: 33, Bits: 6, Literal: 65},
	{Code: 93, Bits: 7, Literal: 66},
	{Code: 94, Bits: 7, Literal: 67},
	{Code: 95, Bits: 7, Literal: 68},
	{Code: 96, Bits: 7, Literal: 69},
	{Code: 97, Bits: 7, Literal: 70},
	{Code: 98, Bits: 7, Literal: 71},
	{Code: 99, Bits: 7, Literal: 72},
	{Code: 100, Bits: 7, Literal: 73},
	{Code: 101, Bits: 7, Literal: 74},
	{Code: 102, Bits: 7, Literal: 75},
	{Code: 103, Bits: 7, Literal: 76},
	{Code: 104, Bits: 7, Literal: 77},
	{Code: 105, Bits: 7, Literal: 78},
	{Code: 106, Bits: 7, Literal: 79},
	{Code: 107, Bits: 7, Literal: 80},
	{Code: 108, Bits: 7, Literal: 81},
	{Code: 109, Bits: 7, Literal: 82},
	{Code: 110, Bits: 7, Literal: 83},
	{Code: 111, Bits: 7, Literal: 84},
	{Code: 112, Bits: 7, Literal: 85},
	{Code: 113, Bits: 7, Literal: 86},
	{Code: 114, Bits: 7, Literal: 87},
	{Code: 252, Bits: 8, Literal: 88},
	{Code: 115, Bits: 7, Literal: 89},
	{Code: 253, Bits: 8, Literal: 90},
	{Code: 8187, Bits: 13, Literal: 91},
	{Code: 524272, Bits: 19, Literal: 92},
	{Code: 8188, Bits: 13, Literal: 93},
	{Code: 16380, Bits: 14, Literal: 94},
	{Code: 34, Bits: 6, Literal: 95},
	{Code: 32765, Bits: 15, Literal: 96},
	{Code: 3, Bits: 5, Literal: 97},
	{Code: 35, Bits: 6, Literal: 98},
	{Code: 4, Bits: 5, Literal: 99},
	{Code: 36, Bits: 6, Literal: 100},
	{Code: 5, Bits: 5, Literal: 101},
	{Code: 37, Bits: 6, Literal: 102},
	{Code: 38, Bits: 6, Literal: 103},
	{Code: 39, Bits: 6, Literal: 104},
	{Code: 6, Bits: 5, Literal: 105},
	{Code: 116, Bits: 7, Literal: 106},
	{Code: 117, Bits: 7, Literal: 107},
	{Code: 40, Bits: 6, Literal: 108},
	{Code: 41, Bits: 6, Literal: 109},
	{Code: 42, Bits: 6, Literal: 110},
	{Code: 7, Bits: 5, Literal: 111},
	{Code: 43, Bits: 6, Literal: 112},
	{Code: 118, Bits: 7, Literal: 113},
	{Code: 44, Bits: 6, Literal: 114},
	{Code: 8, Bits: 5, Literal: 115},
	{Code: 9, Bits: 5, Literal: 116},
	{Code: 45, Bits: 6, Literal: 117},
	{Code: 119, Bits: 7, Literal: 118},
	{Code: 120, Bits: 7, Literal: 119},
	{Code: 121, Bits: 7, Literal: 120},
	{Code: 122, Bits: 7, Literal: 121},
	{Code: 123, Bits: 7, Literal: 122},
	{Code: 32766, Bits: 15, Literal: 123},
	{Code: 2044, Bits: 11, Literal: 124},
	{Code: 16381, Bits: 14, Literal: 125},
	{Code: 8189, Bits: 13, Literal: 126},
	{Code: 268435452, Bits: 28, Literal: 127},
	{Code: 1048550, Bits: 20, Literal: 128},
	{Code: 4194258, Bits: 22, Literal: 129},
	{Code: 1048551, Bits: 20, Literal: 130},
	{Code: 1048552, Bits: 20, Literal: 131},
	{Code: 4194259, Bits: 22, Literal: 132},
	{Code: 4194260, Bits: 22, Literal: 133},
	{Code: 4194261, Bits: 22, Literal: 134},
	{Code: 8388569, Bits: 23, Literal: 135},
	{Code: 4194262, Bits: 22, Literal: 136},
	{Code: 8388570, Bits: 23, Literal: 137},
	{Code: 8388571, Bits: 23, Literal: 138},
	{Code: 8388572, Bits: 23, Literal: 139},
	{Code: 8388573, Bits: 23, Literal: 140},
	{Code: 8388574, Bits: 23, Literal: 141},
	{Code: 16777195, Bits: 24, Literal: 142},
	{Code: 8388575, Bits: 23, Literal: 143},
	{Code: 16777196, Bits: 24, Literal: 144},
	{Code: 16777197, Bits: 24, Literal: 145},
	{Code: 4194263, Bits: 22, Literal: 146},
	{Code: 8388576, Bits: 23, Literal: 147},
	{Code: 16777198, Bits: 24, Literal: 148},
	{Code: 8388577, Bits: 23, Literal: 149},
	{Code: 8388578, Bits: 23, Literal: 150},
	{Code: 8388579, Bits: 23, Literal: 151},
	{Code: 8388580, Bits: 23, Literal: 152},
	{Code: 2097116, Bits: 21, Literal: 153},
	{Code: 4194264, Bits: 22, Literal: 154},
	{Code: 8388581, Bits: 23, Literal: 155},
	{Code: 4194265, Bits: 22, Literal: 156},
	{Code: 8388582, Bits: 23, Literal: 157},
	{Code: 8388583, Bits: 23, Literal: 158},
	{Code: 16777199, Bits: 24, Literal: 159},
	{Code: 4194266, Bits: 22, Literal: 160},
	{Code: 2097117, Bits: 21, Literal: 161},
	{Code: 1048553, Bits: 20, Literal: 162},
	{Code: 4194267, Bits: 22, Literal: 163},
	{Code: 4194268, Bits: 22, Literal: 164},
	{Code: 8388584, Bits: 23, Literal: 165},
	{Code: 8388585, Bits: 23, Literal: 166},
	{Code: 2097118, Bits: 21, Literal: 167},
	{Code: 8388586, Bits: 23, Literal: 168},
	{Code: 4194269, Bits: 22, Literal: 169},
	{Code: 4194270, Bits: 22, Literal: 170},
	{Code: 16777200, Bits: 24, Literal: 171},
	{Code: 2097119, Bits: 21, Literal: 172},
	{Code: 4194271, Bits: 22, Literal: 173},
	{Code: 8388587, Bits: 23, Literal: 174},
	{Code: 8388588, Bits: 23, Literal: 175},
	{Code: 2097120, Bits: 21, Literal: 176},
	{Code: 2097121, Bits: 21, Literal: 177},
	{Code: 4194272, Bits: 22, Literal: 178},
	{Code: 2097122, Bits: 21, Literal: 179},
	{Code: 8388589, Bits: 23, Literal: 180},
	{Code: 4194273, Bits: 22, Literal: 181},
	{Code: 8388590, Bits: 23, Literal: 182},
	{Code: 8388591, Bits: 23, Literal: 183},
	{Code: 1048554, Bits: 20, Literal: 184},
	{Code: 4194274, Bits: 22, Literal: 185},
	{Code: 4194275, Bits: 22, Literal: 186},
	{Code: 4194276, Bits: 22, Literal: 187},
	{Code: 8388592, Bits: 23, Literal: 188},
	{Code: 4194277, Bits: 22, Literal: 189},
	{Code: 4194278, Bits: 22, Literal: 190},
	{Code: 8388593, Bits: 23, Literal: 191},
	{Code: 67108832, Bits: 26, Literal: 192},
	{Code: 67108833, Bits: 26, Literal: 193},
	{Code: 1048555, Bits: 20, Literal: 194},
	{Code: 524273, Bits: 19, Literal: 195},
	{Code: 4194279, Bits: 22, Literal: 196},
	{Code: 8388594, Bits: 23, Literal: 197},
	{Code: 4194280, Bits: 22, Literal: 198},
	{Code: 33554412, Bits: 25, Literal: 199},
	{Code: 67108834, Bits: 26, Literal: 200},
	{Code: 67108835, Bits: 26, Literal: 201},
	{Code: 67108836, Bits: 26, Literal: 202},
	{Code: 134217694, Bits: 27, Literal: 203},
	{Code: 134217695, Bits: 27, Literal: 204},
	{Code: 67108837, Bits: 26, Literal: 205},
	{Code: 16777201, Bits: 24, Literal: 206},
	{Code: 33554413, Bits: 25, Literal: 207},
	{Code: 524274, Bits: 19, Literal: 208},
	{Code: 2097123, Bits: 21, Literal: 209},
	{Code: 67108838, Bits: 26, Literal: 210},
	{Code: 134217696, Bits: 27, Literal: 211},
	{Code: 134217697, Bits: 27, Literal: 212},
	{Code: 67108839, Bits: 26, Literal: 213},
	{Code: 134217698, Bits: 27, Literal: 214},
	{Code: 16777202, Bits: 24, Literal: 215},
	{Code: 2097124, Bits: 21, Literal: 216},
	{Code: 2097125, Bits: 21, Literal: 217},
	{Code: 67108840, Bits: 26, Literal: 218},
	{Code: 67108841, Bits: 26, Literal: 219},
	{Code: 268435453, Bits: 28, Literal: 220},
	{Code: 134217699, Bits: 27, Literal: 221},
	{Code: 134217700, Bits: 27, Literal: 222},
	{Code: 134217701, Bits: 27, Literal: 223},
	{Code: 1048556, Bits: 20, Literal: 224},
	{Code: 16777203, Bits: 24, Literal: 225},
	{Code: 1048557, Bits: 20, Literal: 226},
	{Code: 2097126, Bits: 21, Literal: 227},
	{Code: 4194281, Bits: 22, Literal: 228},
	{Code: 2097127, Bits: 21, Literal: 229},
	{Code: 2097128, Bits: 21, Literal: 230},
	{Code: 8388595, Bits: 23, Literal: 231},
	{Code: 4194282, Bits: 22, Literal: 232},
	{Code: 4194283, Bits: 22, Literal: 233},
	{Code: 33554414, Bits: 25, Literal: 234},
	{Code: 33554415, Bits: 25, Literal: 235},
	{Code: 16777204, Bits: 24, Literal: 236},
	{Code: 16777205, Bits: 24, Literal: 237},
	{Code: 67108842, Bits: 26, Literal: 238},
	{Code: 8388596, Bits: 23, Literal: 239},
	{Code: 67108843, Bits: 26, Literal: 240},
	{Code: 134217702, Bits: 27, Literal: 241},
	{Code: 67108844, Bits: 26, Literal: 242},
	{Code: 67108845, Bits: 26, Literal: 243},
	{Code: 134217703, Bits: 27, Literal: 244},
	{Code: 134217704, Bits: 27, Literal: 245},
	{Code: 134217705, Bits: 27, Literal: 246},
	{Code: 134217706, Bits: 27, Literal: 247},
	{Code: 134217707, Bits: 27, Literal: 248},
	{Code: 268435454, Bits: 28, Literal: 249},
	{Code: 134217708, Bits: 27, Literal: 250},
	{Code: 134217709, Bits: 27, Literal: 251},
	{Code: 134217710, Bits: 27, Literal: 252},
	{Code: 134217711, Bits: 27, Literal: 253},
	{Code: 134217712, Bits: 27, Literal: 254},
	{Code: 67108846, Bits: 26, Literal: 255},
	{Code: 1073741823, Bits: 30, Literal: 256},
}

func HuffmanLength(str []byte) int {
	var len int
	for _, c := range str {
		len += int(Codes[c].Bits)
	}
	return (len + 7) / 8
}
