import { Injectable, HttpException } from '@nestjs/common';
import { DbService } from './db.service';
function invalid(status:number,message:string):never { throw new HttpException({error:message},status); }
function uuid(x:string):boolean { return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(x); }

@Injectable()
export class AccessService {
  constructor(private db:DbService){}
  async authorize(tenantId:string,principalId:string,capabilityId?:string,matterId?:string):Promise<void>{
    if(!uuid(tenantId)||(matterId&&!uuid(matterId))) invalid(400,'Invalid resource identifier');
    await this.db.withTenant(tenantId,async c=>{
      const member=await c.query("SELECT 1 FROM memberships WHERE tenant_id=$1 AND principal_id=$2 AND status='active'",[tenantId,principalId]);
      if(member.rowCount!==1) invalid(403,'Not a member of this tenant');
      if(capabilityId){
        const cap=await c.query('SELECT 1 FROM principal_capabilities WHERE tenant_id=$1 AND principal_id=$2 AND capability_id=$3',[tenantId,principalId,capabilityId]);
        if(cap.rowCount!==1) invalid(403,'Capability not granted');
      }
      if(matterId){
        const access=await c.query("SELECT 1 FROM matter_grants WHERE tenant_id=$1 AND principal_id=$2 AND matter_id=$3 AND permission='matter.read'",[tenantId,principalId,matterId]);
        if(access.rowCount!==1) invalid(403,'Matter access denied');
      }
    });
  }
}

