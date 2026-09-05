export namespace main {
	
	export class DLLDetail {
	    relPath: string;
	    name: string;
	    version: string;
	
	    static createFrom(source: any = {}) {
	        return new DLLDetail(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.relPath = source["relPath"];
	        this.name = source["name"];
	        this.version = source["version"];
	    }
	}
	export class DefenderStatus {
	    appDir: string;
	    excluded: boolean;
	    isAdmin: boolean;
	    available: boolean;
	    exclusions?: string[];
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new DefenderStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.appDir = source["appDir"];
	        this.excluded = source["excluded"];
	        this.isAdmin = source["isAdmin"];
	        this.available = source["available"];
	        this.exclusions = source["exclusions"];
	        this.error = source["error"];
	    }
	}
	export class DownloadURLs {
	    reshade_setup_url: string;
	    reshade_url: string;
	    optiscaler_url: string;
	    dlss5_url: string;
	    dgvoodoo_url: string;
	    dgvoodoo_api: string;
	    feeder_url: string;
	    neural_consumer: string;
	
	    static createFrom(source: any = {}) {
	        return new DownloadURLs(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.reshade_setup_url = source["reshade_setup_url"];
	        this.reshade_url = source["reshade_url"];
	        this.optiscaler_url = source["optiscaler_url"];
	        this.dlss5_url = source["dlss5_url"];
	        this.dgvoodoo_url = source["dgvoodoo_url"];
	        this.dgvoodoo_api = source["dgvoodoo_api"];
	        this.feeder_url = source["feeder_url"];
	        this.neural_consumer = source["neural_consumer"];
	    }
	}
	export class GameDetails {
	    name: string;
	    path: string;
	    executable: string;
	    renderingAPI: string;
	    dlssVersion: string;
	    dlss5Addon: string;
	    reshadeStatus: string;
	    optiScalerStatus: string;
	    dgvoodooStatus: string;
	    recommendsReShade: boolean;
	    is32Bit: boolean;
	    isInstalled: boolean;
	    dllList: DLLDetail[];
	    gpuName: string;
	    neuralSupport: boolean;
	    neuralNote: string;
	    neuralNoteLevel: string;
	    coverArt: string;
	
	    static createFrom(source: any = {}) {
	        return new GameDetails(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.path = source["path"];
	        this.executable = source["executable"];
	        this.renderingAPI = source["renderingAPI"];
	        this.dlssVersion = source["dlssVersion"];
	        this.dlss5Addon = source["dlss5Addon"];
	        this.reshadeStatus = source["reshadeStatus"];
	        this.optiScalerStatus = source["optiScalerStatus"];
	        this.dgvoodooStatus = source["dgvoodooStatus"];
	        this.recommendsReShade = source["recommendsReShade"];
	        this.is32Bit = source["is32Bit"];
	        this.isInstalled = source["isInstalled"];
	        this.dllList = this.convertValues(source["dllList"], DLLDetail);
	        this.gpuName = source["gpuName"];
	        this.neuralSupport = source["neuralSupport"];
	        this.neuralNote = source["neuralNote"];
	        this.neuralNoteLevel = source["neuralNoteLevel"];
	        this.coverArt = source["coverArt"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class GameExeInfo {
	    name: string;
	    path: string;
	    size: number;
	    api: string;
	    isTarget: boolean;
	
	    static createFrom(source: any = {}) {
	        return new GameExeInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.path = source["path"];
	        this.size = source["size"];
	        this.api = source["api"];
	        this.isTarget = source["isTarget"];
	    }
	}
	export class GameInfo {
	    name: string;
	    path: string;
	    exePath: string;
	    isInstalled: boolean;
	    detectedAPI: string;
	
	    static createFrom(source: any = {}) {
	        return new GameInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.path = source["path"];
	        this.exePath = source["exePath"];
	        this.isInstalled = source["isInstalled"];
	        this.detectedAPI = source["detectedAPI"];
	    }
	}
	export class GpuInfo {
	    name: string;
	    supportsNeuralRendering: boolean;
	    vendor: string;
	    vram: number;
	    selected: boolean;
	    active: boolean;
	    nrTier: string;
	
	    static createFrom(source: any = {}) {
	        return new GpuInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.supportsNeuralRendering = source["supportsNeuralRendering"];
	        this.vendor = source["vendor"];
	        this.vram = source["vram"];
	        this.selected = source["selected"];
	        this.active = source["active"];
	        this.nrTier = source["nrTier"];
	    }
	}
	export class PatchResult {
	    success: boolean;
	    message: string;
	
	    static createFrom(source: any = {}) {
	        return new PatchResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.success = source["success"];
	        this.message = source["message"];
	    }
	}
	export class VerifyCheck {
	    name: string;
	    status: string;
	    detail: string;
	    fix: string;
	    autoFixed: boolean;
	
	    static createFrom(source: any = {}) {
	        return new VerifyCheck(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.status = source["status"];
	        this.detail = source["detail"];
	        this.fix = source["fix"];
	        this.autoFixed = source["autoFixed"];
	    }
	}
	export class VerifyReport {
	    success: boolean;
	    gamePath: string;
	    exePath: string;
	    targetDir: string;
	    is32Bit: boolean;
	    api: string;
	    checks: VerifyCheck[];
	    autoFixed: string[];
	    summary: string;
	
	    static createFrom(source: any = {}) {
	        return new VerifyReport(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.success = source["success"];
	        this.gamePath = source["gamePath"];
	        this.exePath = source["exePath"];
	        this.targetDir = source["targetDir"];
	        this.is32Bit = source["is32Bit"];
	        this.api = source["api"];
	        this.checks = this.convertValues(source["checks"], VerifyCheck);
	        this.autoFixed = source["autoFixed"];
	        this.summary = source["summary"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

